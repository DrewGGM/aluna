package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"

	"github.com/DrewGGM/aluna/internal/telemetry"
)

// instrumentation names the meter of the operations metrics that this composition root reads.
const instrumentation = "github.com/DrewGGM/aluna/cmd/aluna"

// registerMetrics registers the operations metrics of the database behind pool and of the pool itself; the returned
// function unregisters them, before the pool closes.
func registerMetrics(pool *pgxpool.Pool) (func() error, error) {
	return telemetry.Register(otel.Meter(instrumentation), databaseState{pool: pool}, func() telemetry.PoolState {
		stat := pool.Stat()
		return telemetry.PoolState{Acquired: int64(stat.AcquiredConns()), Idle: int64(stat.IdleConns()), Max: int64(stat.MaxConns())}
	}, time.Now)
}

// withMetrics registers the metrics of pool and returns what unregisters them, to defer after the pool's close so
// that it runs first; metrics that cannot be registered are logged, and the process goes on without them.
func withMetrics(pool *pgxpool.Pool, log *slog.Logger) func() {
	unregister, err := registerMetrics(pool)
	if err != nil {
		log.Error("register the metrics", "error", err)
		return func() {}
	}
	return func() {
		if err := unregister(); err != nil {
			log.Error("unregister the metrics", "error", err)
		}
	}
}

// databaseState reads the telemetry.State from the modules' tables, read-only, as the composition root that knows
// them: telemetry does not.
type databaseState struct{ pool *pgxpool.Pool }

// Read reads the state. The postings are the inserts into ledger.transactions as the table's statistics count them,
// without scanning it: they include the attempts that rolled back, and they reset with the statistics. Approved
// requests are those a checker approved, whether their command then ran or failed.
func (d databaseState) Read(ctx context.Context) (telemetry.State, error) {
	var s telemetry.State
	var oldestUnsealed *time.Time
	err := d.pool.QueryRow(ctx, `SELECT
			(SELECT coalesce(sum(n_tup_ins), 0)::bigint FROM pg_stat_user_tables WHERE schemaname = 'ledger' AND relname = 'transactions'),
			(SELECT count(*) FROM ledger.holds WHERE status = 'pending'),
			(SELECT count(*) FROM recon.breaks WHERE status = 'open'),
			(SELECT count(*) FROM approvals.requests WHERE status IN ('approved', 'failed')),
			(SELECT recorded_at FROM audit.records WHERE block_id IS NULL ORDER BY seq LIMIT 1)`).
		Scan(&s.Postings, &s.PendingHolds, &s.OpenBreaks, &s.ApprovedRequests, &oldestUnsealed)
	if err != nil {
		return telemetry.State{}, fmt.Errorf("read the operations state: %w", err)
	}
	if oldestUnsealed != nil {
		s.OldestUnsealed = *oldestUnsealed
	}
	// the queues River serves, empty or not, and those with jobs waiting that nobody serves
	rows, err := d.pool.Query(ctx, `SELECT name, sum(n)::bigint FROM (
			SELECT name, 0 AS n FROM infra.river_queue
			UNION ALL
			SELECT queue, count(*) FROM infra.river_job WHERE state IN ('available', 'scheduled', 'retryable') GROUP BY queue
		) depth GROUP BY name`)
	if err != nil {
		return telemetry.State{}, fmt.Errorf("read the queue depth: %w", err)
	}
	s.QueueDepth = map[string]int64{}
	var queue string
	var depth int64
	if _, err := pgx.ForEachRow(rows, []any{&queue, &depth}, func() error {
		s.QueueDepth[queue] = depth
		return nil
	}); err != nil {
		return telemetry.State{}, fmt.Errorf("read the queue depth: %w", err)
	}
	return s, nil
}
