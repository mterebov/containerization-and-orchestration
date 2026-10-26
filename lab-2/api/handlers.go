package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracer creates the nested spans; the root span of every request comes
// from otelhttp.
var tracer = otel.Tracer("api")

func health(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprintln(w, "ok")
}

func fail(w http.ResponseWriter, r *http.Request) {
	err := failOp(r.Context())
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// failOp is the step of /fail that breaks, in its own span marked as failed
// with the reason. The request's span turns red by itself: otelhttp marks a
// server span as failed when the status is 5xx (a 4xx is the client's fault
// and is not), so the waterfall shows a red request and the red step in it.
func failOp(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "fail-op")
	defer span.End()

	err := errors.New("simulated failure")
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	slog.ErrorContext(ctx, "operation failed", "error", err)
	return err
}

func slow(w http.ResponseWriter, r *http.Request) {
	d := time.Duration(1000+rand.IntN(2001)) * time.Millisecond
	if err := slowOp(r.Context(), d); err != nil {
		return // the client went away, there is nobody to answer
	}
	fmt.Fprintf(w, "slept %v\n", d)
}

// slowOp is the slow step of /slow, in its own span: the trace waterfall
// shows that the request's time went exactly here.
func slowOp(ctx context.Context, d time.Duration) error {
	ctx, span := tracer.Start(ctx, "slow-op", trace.WithAttributes(attribute.Int64("sleep_ms", d.Milliseconds())))
	defer span.End()

	select {
	case <-time.After(d):
		slog.InfoContext(ctx, "slow operation finished", "sleep_ms", d.Milliseconds())
		return nil
	case <-ctx.Done():
		span.SetStatus(codes.Error, ctx.Err().Error())
		return ctx.Err()
	}
}

const (
	loadDefault     = 200
	loadMax         = 2000
	loadConcurrency = 20
)

// loader serves /load: it fires a burst of GET /health requests at target
// and waits for all of them. Its client is instrumented too: every request
// carries a traceparent header, so the server spans of the burst land in the
// trace of the /load request that caused it.
type loader struct {
	target string
	client *http.Client
}

func newLoader(target string) *loader {
	return &loader{
		target: strings.TrimSuffix(target, "/") + "/health",
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: otelhttp.NewTransport(&http.Transport{
				// One idle connection per worker; with the default of 2 the
				// burst would keep opening new TCP connections.
				MaxIdleConnsPerHost: loadConcurrency,
			}),
		},
	}
}

func (l *loader) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := loadDefault
	if s := r.URL.Query().Get("n"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 1 || v > loadMax {
			http.Error(w, fmt.Sprintf("n must be an integer from 1 to %d", loadMax), http.StatusBadRequest)
			return
		}
		n = v
	}

	ctx := r.Context()
	start := time.Now()
	var failed atomic.Int64
	var wg sync.WaitGroup
	sem := make(chan struct{}, loadConcurrency)
	for range n {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			if !l.hit(ctx) {
				failed.Add(1)
			}
		}()
	}
	wg.Wait()
	took := time.Since(start)

	nf := int(failed.Load())
	trace.SpanFromContext(ctx).SetAttributes(attribute.Int("load.requests", n), attribute.Int("load.failed", nf))
	slog.InfoContext(ctx, "load burst finished", "target", l.target, "requests", n, "failed", nf, "took_ms", took.Milliseconds())
	fmt.Fprintf(w, "sent %d requests to %s in %v: %d ok, %d failed\n", n, l.target, took.Round(time.Millisecond), n-nf, nf)
}

// hit sends one request and reports whether it got a non-error answer.
func (l *loader) hit(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.target, nil)
	if err != nil {
		return false
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// Read the body to the end, otherwise the connection cannot be reused.
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode < 400
}
