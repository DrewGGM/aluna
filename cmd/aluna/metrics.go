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
// requests are those a checker approved, whether their command then ran or failed. The async entry waiting is the
// next one the aggregator takes, read as the aggregator reads it (AsyncEntriesAfter in the ledger's queries):
// transactions by posted_xact from the cursor, each one's entries by transaction_id, OFFSET 0 keeping the planner
// from entering through the async account and its whole history; the cursor as a scalar subquery, so that the index
// starts at it instead of filtering the whole index against a joined row; the first one, not the min, so that a
// stopped aggregator does not make every collection read its whole backlog.
func (d databaseState) Read(ctx context.Context) (telemetry.State, error) {
	var s telemetry.State
	var oldestUnsealed, oldestUnaggregated *time.Time
	err := d.pool.QueryRow(ctx, `SELECT
			(SELECT coalesce(sum(n_tup_ins), 0)::bigint FROM pg_stat_user_tables WHERE schemaname = 'ledger' AND relname = 'transactions'),
			(SELECT count(*) FROM ledger.holds WHERE status = 'pending'),
			(SELECT count(*) FROM recon.breaks WHERE status = 'open'),
			(SELECT count(*) FROM approvals.requests WHERE status IN ('approved', 'failed')),
			(SELECT recorded_at FROM audit.records WHERE block_id IS NULL ORDER BY seq LIMIT 1),
			(SELECT t.posted_at FROM ledger.transactions t
				CROSS JOIN LATERAL (SELECT id, account_id FROM ledger.entries WHERE transaction_id = t.id OFFSET 0) e
				JOIN ledger.accounts a ON a.id = e.account_id
				WHERE a.balance_mode = 'async'
					AND t.posted_xact >= (SELECT posted_xact FROM ledger.async_cursor)
					AND (t.posted_xact, e.id) > (SELECT posted_xact, entry_id FROM ledger.async_cursor)
				ORDER BY t.posted_xact, e.id LIMIT 1)`).
		Scan(&s.Postings, &s.PendingHolds, &s.OpenBreaks, &s.ApprovedRequests, &oldestUnsealed, &oldestUnaggregated)
	if err != nil {
		return telemetry.State{}, fmt.Errorf("read the operations state: %w", err)
	}
	if oldestUnsealed != nil {
		s.OldestUnsealed = *oldestUnsealed
	}
	if oldestUnaggregated != nil {
		s.OldestUnaggregated = *oldestUnaggregated
	}
	if s.QueueDepth, err = d.queueDepth(ctx); err != nil {
		return telemetry.State{}, err
	}
	if s.ListsRefreshed, err = d.listsRefreshed(ctx); err != nil {
		return telemetry.State{}, err
	}
	return s, nil
}

// queueDepth is how many jobs wait in each queue: the queues River serves, empty or not, and those with jobs
// waiting that nobody serves.
func (d databaseState) queueDepth(ctx context.Context) (map[string]int64, error) {
	rows, err := d.pool.Query(ctx, `SELECT name, sum(n)::bigint FROM (
			SELECT name, 0 AS n FROM infra.river_queue
			UNION ALL
			SELECT queue, count(*) FROM infra.river_job WHERE state IN ('available', 'scheduled', 'retryable') GROUP BY queue
		) depth GROUP BY name`)
	if err != nil {
		return nil, fmt.Errorf("read the queue depth: %w", err)
	}
	depths := map[string]int64{}
	var queue string
	var depth int64
	if _, err := pgx.ForEachRow(rows, []any{&queue, &depth}, func() error {
		depths[queue] = depth
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the queue depth: %w", err)
	}
	return depths, nil
}

// listsRefreshed is when each restrictive list screening reads was last refreshed (ADR-0018): none with the fake
// vendor, which reads none.
func (d databaseState) listsRefreshed(ctx context.Context) (map[string]time.Time, error) {
	rows, err := d.pool.Query(ctx, `SELECT list, refreshed_at FROM compliance.lists`)
	if err != nil {
		return nil, fmt.Errorf("read the restrictive lists: %w", err)
	}
	refreshed := map[string]time.Time{}
	var list string
	var at time.Time
	if _, err := pgx.ForEachRow(rows, []any{&list, &at}, func() error {
		refreshed[list] = at
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the restrictive lists: %w", err)
	}
	return refreshed, nil
}
