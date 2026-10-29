// Command batch stands in for the nightly analytics of the shop in Lab 4:
// background load that may be sacrificed under pressure. It does nothing
// useful. CPU_WORKERS goroutines compute hashes in a loop, and MEMORY_MB of
// memory is allocated and kept resident. A small HTTP server next to the
// load serves the probes and the metrics.
//
//	GET  /health    "ok"
//	GET  /version   the version baked in at build time
//	GET  /metrics   Prometheus metrics
//
// Configuration:
//
//	ADDR            listen address, default ":8082"
//	CPU_WORKERS     goroutines that burn CPU, each up to a whole core, default 1;
//	                0 turns the CPU load off
//	MEMORY_MB       memory to hold, in MiB, default 64; 0 turns the memory load off
//	MEMORY_STEP_MB  MiB added per second until MEMORY_MB is reached, default 16
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// version is set at build time: go build -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	addr := envOr("ADDR", ":8082")
	workers := intEnv("CPU_WORKERS", 1, 0)
	memoryMB := intEnv("MEMORY_MB", 64, 0)
	stepMB := intEnv("MEMORY_STEP_MB", 16, 1)
	// GOMAXPROCS follows the container's CPU limit, so it shows how many
	// cores the workers can actually get at the same time.
	slog.Info("batch starting", "version", version, "addr", addr,
		"cpu_workers", workers, "memory_mb", memoryMB, "memory_step_mb", stepMB,
		"gomaxprocs", runtime.GOMAXPROCS(0))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, version)
	})
	mux.Handle("GET /metrics", promhttp.Handler())

	srv := &http.Server{
		Handler:           mux,
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
	// installs one, so the kubelet's SIGTERM stops the load.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	burnCPU(ctx, workers)
	go holdMemory(ctx, memoryMB, stepMB)
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

	// Unlike api there is nothing to finish: the work is worthless by design.
	// Exiting at once gives the CPU and memory back quickly, which is the
	// point when the pod is preempted or evicted to make room for others.
	shCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(shCtx); err != nil {
		slog.Error("shutdown did not finish in time", "error", err)
	}
	slog.Info("stopped")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// intEnv reads an integer env var, def when unset. A value that is not a
// number or is below lowest will not fix itself, so the process exits: the
// pod crash-loops with the reason in its log.
func intEnv(key string, def, lowest int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lowest {
		slog.Error("invalid integer setting", "key", key, "value", v, "lowest", lowest)
		os.Exit(1)
	}
	return n
}
