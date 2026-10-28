// Command api is the order API of the shop in Lab 3. It takes orders over
// HTTP and stores them in PostgreSQL; the worker then marks them processed.
//
//	GET  /health    "ok", or 503 "fail" when HEALTH_FAIL is set; never touches the database
//	GET  /version   the version baked in at build time
//	POST /order     {"item": "widget", "qty": 1}, both optional -> 201 and the stored order
//	GET  /orders    the latest 100 orders, newest first
//	GET  /metrics   Prometheus metrics
//
// Configuration:
//
//	ADDR           listen address, default ":8080"
//	DATABASE_URL   PostgreSQL URI, e.g. postgresql://user:pass@host:5432/db; empty or
//	               unreachable: the service still starts, and /order and /orders
//	               answer 503 until the database is up
//	HEALTH_FAIL    true, 1 or yes: /health answers 503, a deliberately broken release
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// version is set at build time: go build -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	addr := envOr("ADDR", ":8080")
	dsn := os.Getenv("DATABASE_URL")
	healthFail := truthy(os.Getenv("HEALTH_FAIL"))
	// The URL carries the password: only whether it is set goes to the log.
	slog.Info("api starting", "version", version, "addr", addr,
		"database_url_set", dsn != "", "health_fail", healthFail)

	db, err := openDatabase(dsn, 10)
	if err != nil {
		// A malformed URL will not fix itself, so exit: the pod crash-loops
		// with this line in its log. pgx masks the password in parse errors.
		slog.Error("invalid DATABASE_URL", "error", err)
		os.Exit(1)
	}
	if dsn == "" {
		slog.Warn("DATABASE_URL is not set: POST /order and GET /orders will answer 503")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", health(healthFail))
	mux.HandleFunc("GET /version", showVersion)
	mux.HandleFunc("POST /order", createOrder(db))
	mux.HandleFunc("GET /orders", listOrders(db))
	mux.Handle("GET /metrics", promhttp.Handler())

	srv := &http.Server{
		Handler:           instrument(mux),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("listen failed", "addr", addr, "error", err)
		os.Exit(1)
	}
	slog.Info("listening", "addr", ln.Addr().String())

	// In a container this process is PID 1, and the kernel drops signals sent
	// to a namespace's PID 1 unless it has a handler for them. NotifyContext
	// installs one, so the kubelet's SIGTERM starts a graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	watchDone := db.watch(ctx)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		slog.Error("serve failed", "error", err)
		os.Exit(1)
	case <-ctx.Done():
	}
	stop() // from here on a second Ctrl-C kills the process at once
	slog.Info("shutting down")

	// Shutdown closes the listener, so no new connections are accepted, and
	// waits for the requests in progress to finish: a pod replaced in a
	// rolling update completes what it has already accepted instead of
	// cutting it off. 10 s stays inside the default 30 s
	// terminationGracePeriodSeconds, after which the kubelet sends SIGKILL.
	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shCtx); err != nil {
		slog.Error("shutdown did not finish in time", "error", err)
	}
	<-watchDone
	db.close() // only now: the last requests may have been using it
	slog.Info("stopped")
}

// health answers the kubelet's probes. It deliberately does not look at the
// database. If it did, the pods could not become ready before the database
// exists, and a database outage would fail the probes of every replica at
// once: readiness would pull them all out of the Service, liveness would
// restart them, and neither fixes the database. HEALTH_FAIL simulates a
// broken release.
func health(fail bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if fail {
			http.Error(w, "fail", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	}
}

func showVersion(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprintln(w, version)
}

// Metrics of the HTTP layer. Label values must come from small fixed sets:
// every distinct combination is a separate time series in Prometheus.
var (
	httpRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "HTTP requests served, by method, route and status code.",
	}, []string{"method", "route", "code"})
	httpDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "Time to serve an HTTP request, by method and route.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "route"})
)

// instrument wraps the whole mux, so every request is counted, timed and
// logged, including the ones that match no route (404, 405).
func instrument(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route, method := routeOf(mux, r), methodOf(r)
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		mux.ServeHTTP(rec, r)
		took := time.Since(start)

		httpRequests.WithLabelValues(method, route, strconv.Itoa(rec.code)).Inc()
		httpDuration.WithLabelValues(method, route).Observe(took.Seconds())

		// The kubelet probes every pod every few seconds; a log line per
		// probe would bury the real traffic. Probes are still counted above.
		if strings.HasPrefix(r.UserAgent(), "kube-probe/") {
			return
		}
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "route", route,
			"code", rec.code, "duration_ms", float64(took.Microseconds())/1000)
	})
}

// routeOf returns the path of the pattern the mux serves r with
// ("POST /order" -> "/order"), or "other" when r matches none. Unlike
// r.URL.Path, which is whatever the client sent, this is a fixed set.
func routeOf(mux *http.ServeMux, r *http.Request) string {
	if _, pattern := mux.Handler(r); pattern != "" {
		if _, path, ok := strings.Cut(pattern, " "); ok {
			return path
		}
	}
	return "other"
}

// methodOf folds methods other than the standard ones into "other": the
// method is client input too.
func methodOf(r *http.Request) string {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return r.Method
	}
	return "other"
}

// statusRecorder remembers the status code a handler sent: 200 unless it
// called WriteHeader with something else.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// truthy reports whether an env var is switched on: true, 1 or yes, in any case.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes":
		return true
	}
	return false
}
