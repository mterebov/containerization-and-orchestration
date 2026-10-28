package main

import (
	"context"
	"database/sql"
	"log/slog"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	ordersProcessed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "worker_orders_processed_total",
		Help: "Orders this worker has marked processed.",
	})
	pollErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "worker_poll_errors_total",
		Help: "Polls that failed with a database error.",
	})
	lastPoll = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "worker_last_poll_timestamp_seconds",
		Help: "Unix time of the last successful poll, 0 before the first one.",
	})
)

// claimOrders marks up to $1 new orders processed and returns their ids.
// Picking the rows and updating them is one statement, so it is atomic.
// Several worker replicas run it concurrently: FOR UPDATE locks the rows the
// subquery picks, and SKIP LOCKED makes another worker pass over rows that
// are already locked instead of waiting for them. So the replicas take
// disjoint batches in parallel, and no order is processed twice.
const claimOrders = `UPDATE orders SET status = 'processed', processed_at = now()
	WHERE id IN (
		SELECT id FROM orders WHERE status = 'new' ORDER BY id LIMIT $1
		FOR UPDATE SKIP LOCKED
	)
	RETURNING id`

type worker struct {
	db        *database
	batchSize int
	failing   bool // inside a streak of failed polls: only its first error is logged
}

// run polls every interval until ctx is cancelled. Cancellation is looked at
// only between polls: on SIGTERM the poll in progress is finished, then the
// loop returns.
func (w *worker) run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.poll()
		}
	}
}

func (w *worker) poll() {
	pool, err := w.db.available()
	if err != nil {
		return // not configured or down: nothing to do, db_up says why
	}
	// Deliberately not the loop's context: SIGTERM stops the loop from
	// starting another poll, but must not abort the statement midway, which
	// would only show up as a spurious poll error at every shutdown.
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	ids, err := claim(ctx, pool, w.batchSize)
	if err != nil {
		pollErrors.Inc()
		if !w.failing {
			slog.Warn("poll failed", "error", err)
			w.failing = true
		}
		return
	}
	lastPoll.SetToCurrentTime()
	if w.failing {
		slog.Info("poll recovered")
		w.failing = false
	}
	if len(ids) > 0 {
		ordersProcessed.Add(float64(len(ids)))
		slog.Info("orders processed", "count", len(ids), "ids", ids)
	}
}

func claim(ctx context.Context, pool *sql.DB, limit int) ([]int64, error) {
	rows, err := pool.QueryContext(ctx, claimOrders, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.Sort(ids) // RETURNING comes in no particular order
	return ids, nil
}
