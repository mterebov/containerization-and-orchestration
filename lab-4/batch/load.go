package main

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	work = promauto.NewCounter(prometheus.CounterOpts{
		Name: "batch_work_total",
		Help: "Units of useless work done: SHA-256 hashes computed by the CPU workers.",
	})
	memoryHeld = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "batch_memory_held_bytes",
		Help: "Memory allocated and written to, so resident in RAM.",
	})
	memoryTarget = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "batch_memory_target_bytes",
		Help: "Memory the process is told to hold: MEMORY_MB.",
	})
)

// hashesPerReport is how many hashes a worker computes between looking at
// the context and adding to the counter: doing both on every hash would cost
// more than the hash itself.
const hashesPerReport = 1 << 14

// burnCPU starts n goroutines that hash in a tight loop until ctx is done.
// Each one keeps a core busy for as long as the kernel lets it. Under a CPU
// limit the container is throttled, and when the node is short of CPU it
// gets the share its request buys: the rate of batch_work_total shows how
// much work gets through either way.
func burnCPU(ctx context.Context, n int) {
	for range n {
		go func() {
			sum := sha256.Sum256(nil)
			for ctx.Err() == nil {
				for range hashesPerReport {
					sum = sha256.Sum256(sum[:])
				}
				work.Add(hashesPerReport)
			}
		}()
	}
}

const (
	mib = 1 << 20
	// pageSize is the unit in which the kernel backs memory with RAM: a fresh
	// allocation costs nothing until it is written, and one byte per page is
	// enough to make the whole page resident.
	pageSize = 4096
)

// holdMemory allocates targetMB MiB, stepMB MiB a second, writes into every
// page and keeps the memory until ctx is done. Pod containers may not swap,
// so what is written stays in RAM and in the working set the kubelet watches.
//
// The ramp is deliberate. The kubelet looks at node memory every ten
// seconds; memory that grows slowly lets it notice the pressure and evict
// pods itself, before the node runs out and the kernel OOM killer acts.
// A target above the container's own memory limit is the other case: the
// kernel kills the container at the limit, and the pod shows OOMKilled.
func holdMemory(ctx context.Context, targetMB, stepMB int) {
	memoryTarget.Set(float64(targetMB) * mib)
	chunks := make([][]byte, 0, targetMB)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		if len(chunks) < targetMB {
			for i := 0; i < stepMB && len(chunks) < targetMB; i++ {
				c := make([]byte, mib)
				for p := 0; p < len(c); p += pageSize {
					c[p] = 1
				}
				chunks = append(chunks, c)
			}
			memoryHeld.Set(float64(len(chunks)) * mib)
			if len(chunks) == targetMB {
				slog.Info("memory target reached", "memory_mb", targetMB)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
