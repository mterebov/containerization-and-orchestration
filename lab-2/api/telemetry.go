package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// setupTracing installs the global TracerProvider and propagator. Spans are
// always created, so every request has a trace_id for the logs; they are
// exported only when an OTLP endpoint is set. The exporter reads the standard
// OTEL_EXPORTER_OTLP_* variables itself.
func setupTracing(ctx context.Context) (shutdown func(context.Context) error, exporting bool, err error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName("api")), // OTEL_SERVICE_NAME overrides it
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithHost(), // host.name: the pod name in Kubernetes
	)
	if err != nil {
		return nil, false, err
	}

	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" {
		exp, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, false, err
		}
		// The batcher keeps finished spans in memory and sends them in the
		// background, so a request never waits for the receiver. Whatever is
		// still buffered when the process is killed is lost.
		opts = append(opts, sdktrace.WithBatcher(exp))
		exporting = true
	}
	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)

	// The global propagator is a no-op until set: without this, traceparent
	// would be neither read from incoming requests nor added to outgoing ones.
	otel.SetTextMapPropagator(propagation.TraceContext{})

	// Export errors (receiver down, say) would otherwise go to log.Print and
	// show up as INFO lines; make them warnings with the error in its own field.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		slog.Warn("opentelemetry error", "error", err)
	}))
	return tp.Shutdown, exporting, nil
}

// traceHandler adds the trace_id and span_id of the span in ctx to every log
// record: that is what links a log line in Loki to its trace in Jaeger. It
// only sees the span if the caller passes ctx, as slog.InfoContext does.
type traceHandler struct{ slog.Handler }

func (h traceHandler) Handle(ctx context.Context, rec slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		rec.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, rec)
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{h.Handler.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{h.Handler.WithGroup(name)}
}

// metrics are the RED metrics of the service: rate and errors from the
// counters, duration from the histogram.
type metrics struct {
	requests *prometheus.CounterVec
	errors   *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func newMetrics(reg *prometheus.Registry) *metrics {
	// Go runtime and process metrics: goroutines, GC, RSS, CPU, open fds.
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	f := promauto.With(reg)
	return &metrics{
		requests: f.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "HTTP requests handled, by method, route and status code.",
		}, []string{"method", "route", "code"}),
		errors: f.NewCounterVec(prometheus.CounterOpts{
			Name: "http_request_errors_total",
			Help: "HTTP requests answered with a 5xx status, by route.",
		}, []string{"route"}),
		duration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name: "http_request_duration_seconds",
			Help: "Time from receiving a request to finishing its response, by method, route and status code.",
			// histogram_quantile interpolates linearly inside a bucket, so a
			// quantile is only as precise as the buckets around it. The
			// default buckets jump from 2.5 to 5 s: for a stream of /slow
			// alone (uniform 1-3 s) p95 would come out at 4.5 s instead of
			// 2.9 s. The extra boundaries up to 3 s keep it close.
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 1.5, 2, 2.5, 3, 5, 10},
		}, []string{"method", "route", "code"}),
	}
}

// instrument wraps the handler registered under pattern ("GET /slow") with
// tracing, the RED metrics and the access log.
func (m *metrics) instrument(pattern string, h http.Handler) http.Handler {
	route := pattern
	if _, path, ok := strings.Cut(pattern, " "); ok {
		route = path
	}
	// A counter that was never incremented does not exist yet, and rate() of
	// a missing series is "no data", not 0. Creating the error series up
	// front makes the error ratio read 0 before the first failure.
	m.errors.WithLabelValues(route)

	observed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		took := time.Since(start)

		code := strconv.Itoa(rec.status)
		m.requests.WithLabelValues(r.Method, route, code).Inc()
		if rec.status >= 500 {
			m.errors.WithLabelValues(route).Inc()
		}
		obs := m.duration.WithLabelValues(r.Method, route, code)
		if sc := trace.SpanContextFromContext(r.Context()); sc.IsSampled() {
			// An exemplar pins this observation to its trace, so Grafana can
			// jump from a point on the latency graph to the trace in Jaeger.
			obs.(prometheus.ExemplarObserver).ObserveWithExemplar(took.Seconds(), prometheus.Labels{"trace_id": sc.TraceID().String()})
		} else {
			obs.Observe(took.Seconds())
		}

		if isProbe(r) {
			return
		}
		level := slog.LevelInfo
		if rec.status >= 500 {
			level = slog.LevelError
		}
		slog.Log(r.Context(), level, "request",
			"method", r.Method, "path", r.URL.Path, "route", route,
			"status", rec.status, "duration_ms", float64(took.Microseconds())/1000)
	})

	// The kubelet probes /health every few seconds. A trace and a log line
	// per probe would bury the real requests, so probes are only counted.
	return otelhttp.NewHandler(observed, pattern, otelhttp.WithFilter(func(r *http.Request) bool { return !isProbe(r) }))
}

// isProbe reports whether r is a kubelet liveness/readiness/startup probe:
// the kubelet sends them with User-Agent "kube-probe/<version>".
func isProbe(r *http.Request) bool {
	return strings.HasPrefix(r.UserAgent(), "kube-probe/")
}

// statusRecorder remembers the status code a handler sent.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}
