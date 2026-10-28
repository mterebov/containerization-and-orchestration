package main

// This file is the same in api and worker.

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// schema is created by both api and worker, whichever reaches the database
// first; IF NOT EXISTS makes it a no-op for the other one.
const schema = `CREATE TABLE IF NOT EXISTS orders (
	id           BIGSERIAL PRIMARY KEY,
	item         TEXT NOT NULL,
	qty          INT NOT NULL CHECK (qty > 0),
	status       TEXT NOT NULL DEFAULT 'new',
	created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
	processed_at TIMESTAMPTZ
)`

// schemaLock is the key of the advisory lock taken around the DDL; any
// number works as long as nothing else in the database uses it.
const schemaLock = 7_320_001

const (
	checkEvery    = 5 * time.Second
	pingTimeout   = 2 * time.Second
	schemaTimeout = 10 * time.Second
	queryTimeout  = 5 * time.Second
)

var (
	errNoDatabase   = errors.New("DATABASE_URL is not set")
	errDatabaseDown = errors.New(`database is down, see the last "database down" log line`)
)

// database is the connection pool plus what the last check of it found.
type database struct {
	pool *sql.DB     // nil when DATABASE_URL is not set
	up   atomic.Bool // written only by watch; read by requests and by db_up
}

// openDatabase parses dsn and prepares a pool, without connecting: sql.DB
// opens connections lazily, on first use, so the service starts whether or
// not the database is reachable. An empty dsn gives a database that is never
// up. The pool is capped at maxConns: every replica has its own pool, and
// all of them together must stay below Postgres' max_connections (100 by
// default).
func openDatabase(dsn string, maxConns int) (*database, error) {
	d := &database{}
	promauto.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "db_up",
		Help: "1 if the last database check succeeded (connection and schema), 0 otherwise.",
	}, func() float64 {
		if d.up.Load() {
			return 1
		}
		return 0
	})
	if dsn == "" {
		return d, nil
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	d.pool = stdlib.OpenDB(*cfg)
	d.pool.SetMaxOpenConns(maxConns)
	return d, nil
}

// available returns the pool if the last check found the database usable.
// While it is down this fails at once, so a request gets its 503 right away
// instead of waiting for a connect timeout.
func (d *database) available() (*sql.DB, error) {
	switch {
	case d.pool == nil:
		return nil, errNoDatabase
	case !d.up.Load():
		return nil, errDatabaseDown
	}
	return d.pool, nil
}

// watch checks the database every checkEvery until ctx is cancelled and
// keeps up current. It logs the first result and then only changes, not
// every check. The returned channel is closed once it has stopped.
func (d *database) watch(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	if d.pool == nil {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		t := time.NewTicker(checkEvery)
		defer t.Stop()
		logged := false
		for {
			err := d.check(ctx)
			if ctx.Err() != nil {
				return // shutting down: we cut the check off, not the database
			}
			up := err == nil
			if was := d.up.Swap(up); !logged || was != up {
				if up {
					slog.Info("database up")
				} else {
					slog.Warn("database down", "error", err)
				}
				logged = true
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return done
}

// check pings the database and, when it is coming up (at start or after an
// outage), makes sure the schema exists: the database may be brand new, for
// example recreated by its operator.
func (d *database) check(ctx context.Context) error {
	if err := d.ping(ctx); err != nil {
		return err
	}
	if d.up.Load() {
		return nil
	}
	return d.ensureSchema(ctx)
}

func (d *database) ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	return d.pool.PingContext(ctx)
}

// ensureSchema runs the DDL. Several replicas of api and worker may get here
// at the same moment, and two concurrent CREATE TABLE IF NOT EXISTS can
// still collide in the system catalog with a unique violation instead of a
// no-op. A transaction-scoped advisory lock makes them take turns.
func (d *database) ensureSchema(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, schemaTimeout)
	defer cancel()
	tx, err := d.pool.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // a no-op after Commit
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", schemaLock); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *database) close() {
	if d.pool != nil {
		d.pool.Close()
	}
}
