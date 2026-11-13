//go:build integration

package main

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// appPool is a pool of the application's role, as in production.
func appPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	appDSN, err := pgtest.AppDSN(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := postgres.Open(t.Context(), appDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// asOwner runs the statements as the tables' owner.
func asOwner(t *testing.T, pool *pgxpool.Pool, statements ...string) {
	t.Helper()
	pgtest.AsOwner(t, pool, func(tx pgx.Tx) error {
		for _, sql := range statements {
			if _, err := tx.Exec(t.Context(), sql); err != nil {
				return err
			}
		}
		return nil
	})
}

// TestTheOperationsStateIsReadAsTheApplication: the application's role reads every count of the operations metrics;
// an open break and an audit record waiting for its seal show up in them; and a queue has its depth whether River
// serves it or only has jobs waiting in it.
func TestTheOperationsStateIsReadAsTheApplication(t *testing.T) {
	pool := appPool(t)
	before, err := databaseState{pool: pool}.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	asOwner(t, pool,
		`INSERT INTO recon.breaks (id, legal_entity_id, control, subject, currency, difference_units)
			VALUES ('`+ids.New().String()+`', '`+ids.New().String()+`', 'trial_balance', 'COP', 'COP', 1)`,
		`INSERT INTO infra.river_queue (name, metadata, created_at, updated_at) VALUES ('metrics-served', '{}', now(), now())
			ON CONFLICT DO NOTHING`,
		`INSERT INTO infra.river_job (args, kind, max_attempts, queue, state, scheduled_at)
			VALUES ('{}', 'metrics.test', 1, 'metrics-unserved', 'available', now() + interval '1 year')`,
		`INSERT INTO audit.records (actor, role, channel, ip, correlation_id, command, targets, result)
			VALUES ('tester', 'operator', 'api', '127.0.0.1', 'metrics-test', 'POST /v1/test', '{}', 'ok')`)

	after, err := databaseState{pool: pool}.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	served, servedKnown := after.QueueDepth["metrics-served"]
	if after.OpenBreaks != before.OpenBreaks+1 || !servedKnown || served != 0 || after.QueueDepth["metrics-unserved"] < 1 ||
		after.OldestUnsealed.IsZero() {
		t.Fatalf("before %+v, after %+v", before, after)
	}
}
