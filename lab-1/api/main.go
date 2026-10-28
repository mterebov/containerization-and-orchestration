// Command api is a small HTTP service used as a test subject in the
// containerization lab: it is run inside namespaces, under cgroup limits and
// with a seccomp profile, and poked from outside.
//
//	GET /health      liveness check, answers "ok"
//	GET /eat?mb=N    allocate N MiB more and hold them forever
//	GET /burn        start one more goroutine spinning on a CPU core
//	GET /whoami      pid, ppid, uid, gid, hostname, GOMAXPROCS, NumCPU
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", envOr("ADDR", ":8080"), "listen address (env ADDR)")
	flag.Parse()

	// In its own pid namespace this process is PID 1, and the kernel drops a
	// signal sent to a namespace's PID 1 unless PID 1 has a handler for it
	// (only SIGKILL/SIGSTOP from the parent namespace always get through).
	// The explicit handler makes SIGTERM/SIGINT arrive and trigger a graceful
	// shutdown with exit code 0. Without it the Go runtime would catch the
	// signal itself, fail to re-raise it on PID 1 and fall back to a hard exit.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", getOnly(health))
	mux.HandleFunc("/eat", getOnly(eat))
	mux.HandleFunc("/burn", getOnly(burn))
	mux.HandleFunc("/whoami", getOnly(whoami))

	srv := &http.Server{
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 5 * time.Second,
		// No WriteTimeout on purpose: it would cut off slow /eat responses.
	}

	// A wildcard address (":8080") needs no interface to be up, so this also
	// works in a fresh network namespace where lo is still down. Binding a
	// concrete IP such as 127.0.0.1 would fail there.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen %s: %v", *addr, err)
	}
	log.Printf("api listening on %s pid=%d", *addr, os.Getpid())

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		log.Fatalf("serve: %v", err)
	case sig := <-sigs:
		log.Printf("got %v, shutting down", sig)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	log.Print("bye")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// statusRecorder remembers the status code a handler sent, for the log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// logRequests logs one line per request: method, URI, status, duration.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s %d %s", r.Method, r.RequestURI, rec.status, time.Since(start))
	})
}

// getOnly answers 405 to every method except GET. A "GET /path" mux pattern
// would also accept HEAD, and a HEAD must not allocate memory or start burners.
func getOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		h(w, r)
	}
}

func health(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprintln(w, "ok")
}

// Memory taken by /eat. The slices stay reachable from this package-level
// variable, so the GC can never free them.
var (
	heldMu sync.Mutex
	held   [][]byte
	heldMB int
)

// maxEatMB is the largest mb for which mb<<20 still fits in an int.
const maxEatMB = math.MaxInt >> 20

func eat(w http.ResponseWriter, r *http.Request) {
	mb, err := strconv.Atoi(r.URL.Query().Get("mb"))
	if err != nil || mb < 1 {
		http.Error(w, "mb must be an integer >= 1", http.StatusBadRequest)
		return
	}
	if mb > maxEatMB {
		http.Error(w, "mb is too large", http.StatusBadRequest)
		return
	}

	buf := make([]byte, mb<<20)
	// Touch every page. A big allocation usually comes straight from fresh
	// mmap'd memory that the kernel has already zeroed, so Go hands it out
	// without writing to it. Untouched pages are never faulted in: they are
	// not resident and not charged to the cgroup, so without this loop
	// memory.current would not grow and the OOM experiment would not work.
	// The stride is 4096, the smallest page size, so 16K/64K pages are hit
	// too. Past memory.max, the kernel OOM-kills the whole process somewhere
	// in this loop, mid-request: that is the expected outcome.
	for i := 0; i < len(buf); i += 4096 {
		buf[i] = 1
	}

	heldMu.Lock()
	held = append(held, buf)
	heldMB += mb
	total := heldMB
	heldMu.Unlock()

	fmt.Fprintf(w, "ate %d MB, holding %d MB total\n", mb, total)
}

var (
	burners atomic.Int64  // goroutines started by /burn
	spins   atomic.Uint64 // bumped by every iteration of every burn loop
)

func burn(w http.ResponseWriter, _ *http.Request) {
	n := burners.Add(1)
	go func() {
		// Pin this goroutine to one OS thread for good, so the load shows
		// up as a single thread at ~100% in top -H and in per-thread stats
		// (/proc/<pid>/task/<tid>) instead of hopping between threads.
		runtime.LockOSThread()
		// An atomic add is real work the compiler cannot remove; no sleeps,
		// no syscalls. Go's async preemption still interrupts this loop, so
		// the server stays responsive.
		for {
			spins.Add(1)
		}
	}()
	fmt.Fprintf(w, "burning: %d goroutines busy\n", n)
}

func whoami(w http.ResponseWriter, _ *http.Request) {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown (" + err.Error() + ")"
	}
	fmt.Fprintf(w, "pid=%d\nppid=%d\nuid=%d\ngid=%d\nhostname=%s\ngomaxprocs=%d\nnumcpu=%d\n",
		os.Getpid(), os.Getppid(), os.Getuid(), os.Getgid(), host,
		runtime.GOMAXPROCS(0), runtime.NumCPU())
}
