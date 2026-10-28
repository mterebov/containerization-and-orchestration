package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var ordersCreated = promauto.NewCounter(prometheus.CounterOpts{
	Name: "orders_created_total",
	Help: "Orders stored by POST /order.",
})

// order is a row of the orders table as the API returns it.
type order struct {
	ID          int64      `json:"id"`
	Item        string     `json:"item"`
	Qty         int        `json:"qty"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	ProcessedAt *time.Time `json:"processed_at"` // null until the worker has processed it
}

const (
	insertOrder = `INSERT INTO orders (item, qty) VALUES ($1, $2)
		RETURNING id, item, qty, status, created_at, processed_at`
	selectOrders = `SELECT id, item, qty, status, created_at, processed_at
		FROM orders ORDER BY id DESC LIMIT 100`
)

// createOrder serves POST /order. The body is optional, the defaults are
// {"item": "widget", "qty": 1}.
func createOrder(db *database) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Item string `json:"item"`
			Qty  *int   `json:"qty"` // a pointer tells a missing qty (default 1) from an explicit 0
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		// A typo like "quantity" is an error rather than a silent default.
		dec.DisallowUnknownFields()
		// io.EOF is an empty body: all defaults.
		if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		item := strings.TrimSpace(req.Item)
		if item == "" {
			item = "widget"
		}
		qty := 1
		if req.Qty != nil {
			qty = *req.Qty
		}
		switch {
		case qty <= 0:
			writeError(w, http.StatusBadRequest, "qty must be > 0")
			return
		case qty > math.MaxInt32: // the column is INT
			writeError(w, http.StatusBadRequest, "qty is too large")
			return
		}

		pool, err := db.available()
		if err != nil {
			failDB(w, "/order", err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
		defer cancel()
		o, err := scanOrder(pool.QueryRowContext(ctx, insertOrder, item, qty))
		if err != nil {
			failDB(w, "/order", err)
			return
		}
		ordersCreated.Inc()
		writeJSON(w, http.StatusCreated, o)
	}
}

// listOrders serves GET /orders: the latest 100 orders, newest first.
func listOrders(db *database) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pool, err := db.available()
		if err != nil {
			failDB(w, "/orders", err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
		defer cancel()
		rows, err := pool.QueryContext(ctx, selectOrders)
		if err != nil {
			failDB(w, "/orders", err)
			return
		}
		defer rows.Close()

		orders := []order{} // encodes as [] rather than null when there are none
		for rows.Next() {
			o, err := scanOrder(rows)
			if err != nil {
				failDB(w, "/orders", err)
				return
			}
			orders = append(orders, o)
		}
		if err := rows.Err(); err != nil {
			failDB(w, "/orders", err)
			return
		}
		writeJSON(w, http.StatusOK, orders)
	}
}

// scanOrder reads one row of the columns both queries return.
func scanOrder(row interface{ Scan(...any) error }) (order, error) {
	var o order
	err := row.Scan(&o.ID, &o.Item, &o.Qty, &o.Status, &o.CreatedAt, &o.ProcessedAt)
	return o, err
}

// failDB answers a request whose database work failed: 503 if the database
// cannot serve right now (the client may retry later), 500 otherwise.
func failDB(w http.ResponseWriter, route string, err error) {
	if isUnavailable(err) {
		slog.Warn("database unavailable", "route", route, "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	slog.Error("database query failed", "route", route, "error", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

// isUnavailable tells "the database cannot serve right now" from a query
// that failed. An error that is not a *pgconn.PgError never got an answer
// from the server at all: not configured, known to be down, dial error,
// timeout, dropped connection. Among the server's own errors, a few classes
// also mean "not now" rather than "wrong query".
func isUnavailable(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return true
	}
	code := pgErr.Code
	return strings.HasPrefix(code, "08") || // connection exception
		strings.HasPrefix(code, "53") || // insufficient resources, e.g. too many connections
		strings.HasPrefix(code, "57") || // operator intervention: shutting down, starting up
		code == "25006" // read-only transaction: the connection landed on a replica after a failover
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // read in a terminal, not embedded in HTML: keep ">" as is
	enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
