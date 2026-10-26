// Command api is the service under test for the monitoring lab. It exposes
// RED metrics for Prometheus, writes JSON logs to stdout that carry the
// current trace_id, and sends OpenTelemetry traces over OTLP/HTTP. Each
// endpoint provokes one situation for the dashboards and alerts to catch.
//
//	GET /health      liveness/readiness check, answers "ok"
//	GET /fail        answers 500; the nested span "fail-op" is marked as failed
//	GET /slow        sleeps 1-3 s inside the nested span "slow-op"
//	GET /load?n=N    fires N requests (default 200, max 2000) at GET /health
//	GET /metrics     Prometheus metrics
//
// Configuration, all optional:
//
//	ADDR                          listen address, default ":8080"
//	LOAD_TARGET                   base URL /load fires at; default is this
//	                              process over loopback, http://api:8080 would
//	                              go through the Service and spread over pods
//	OTEL_EXPORTER_OTLP_ENDPOINT   OTLP/HTTP receiver, e.g. http://jaeger:4318;
//	                              unset: spans get trace_ids but are not sent
//	OTEL_SERVICE_NAME             service.name of the traces, default "api"
package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	slog.SetDefault(slog.New(traceHandler{slog.NewJSONHandler(os.Stdout, nil)}))

	addr := envOr("ADDR", ":8080")
	loadTarget := envOr("LOAD_TARGET", selfURL(addr))

	// In a container this process is PID 1, and the kernel drops signals sent
	// to a namespace's PID 1 unless it has a handler for them. NotifyContext
	// installs one, so the kubelet's SIGTERM triggers a graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	shutdownTracing, exporting, err := setupTracing(ctx)
	if err != nil {
		slog.Error("tracing setup failed", "error", err)
		os.Exit(1)
	}

	reg := prometheus.NewRegistry()
	m := newMetrics(reg)

	mux := http.NewServeMux()
	handle := func(pattern string, h http.Handler) { mux.Handle(pattern, m.instrument(pattern, h)) }
	handle("GET /health", http.HandlerFunc(health))
	handle("GET /fail", http.HandlerFunc(fail))
	handle("GET /slow", http.HandlerFunc(slow))
	handle("GET /load", newLoader(loadTarget))
	// Registered bare: a scrape is not traffic, so it is not traced, counted
	// or logged. OpenMetrics is the text format that can carry exemplars.
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{EnableOpenMetrics: true}))

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("listen failed", "addr", addr, "error", err)
		os.Exit(1)
	}
	slog.Info("api listening", "addr", addr, "load_target", loadTarget, "exporting_spans", exporting)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		slog.Error("serve failed", "error", err)
		os.Exit(1)
	case <-ctx.Done():
		slog.Info("shutting down")
	}

	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shCtx); err != nil {
		slog.Error("shutdown failed", "error", err)
	}
	// Only after the server has drained: the spans of the last requests are
	// still in the batcher's buffer, and this flushes them.
	if err := shutdownTracing(shCtx); err != nil {
		slog.Error("flushing spans failed", "error", err)
	}
	slog.Info("bye")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// selfURL turns a listen address like ":8080" into a URL that reaches this
// same process over loopback, so inside a pod the load never leaves the pod.
func selfURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://127.0.0.1:8080"
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}
