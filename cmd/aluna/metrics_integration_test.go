//go:build integration

package main

import (
	"testing"
	"time"

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

// TestTheListsAgeIsReadAsTheApplication (Q-145): when each restrictive list was refreshed shows up, as the
// application's role reads it.
func TestTheListsAgeIsReadAsTheApplication(t *testing.T) {
	pool := appPool(t)
	asOwner(t, pool, `INSERT INTO compliance.lists (list, version, entries, refreshed_at)
		VALUES ('un_consolidated', 'v1', 1, '2026-03-02 10:00:00+00') ON CONFLICT (list) DO UPDATE SET refreshed_at = EXCLUDED.refreshed_at`)

	s, err := databaseState{pool: pool}.Read(t.Context())
	if err != nil || !s.ListsRefreshed["un_consolidated"].Equal(time.Date(2026, time.March, 2, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("lists refreshed = %v (%v), want the UN list's refresh", s.ListsRefreshed, err)
	}
}

// TestTheStuckEventsAndHoldsAreReadAsTheApplication (Q-79, Q-60): as the application's role reads them, an event
// already published, a hold already expired and a pending hold not yet due are not stuck; an event not yet published
// and a pending hold past its expiry are, and, older than anything the other tests of this package leave, they are
// the oldest.
func TestTheStuckEventsAndHoldsAreReadAsTheApplication(t *testing.T) {
	pool := appPool(t)
	entity, cash, deposits := ids.New().String(), ids.New().String(), ids.New().String()
	event := func(createdAt, publishedAt string) string {
		return `INSERT INTO infra.outbox (id, type, subject, legal_entity_id, schema_version, payload, created_at, published_at)
			VALUES ('` + ids.New().String() + `', 'aluna.ledger.hold.expired.v1', 'metrics', '` + entity + `', 1, '{}', ` + createdAt + `, ` + publishedAt + `)`
	}
	hold := func(status, expiresAt string) string {
		return `INSERT INTO ledger.holds (id, legal_entity_id, account_id, counter_account_id, currency, amount, status, expires_at)
			VALUES ('` + ids.New().String() + `', '` + entity + `', '` + deposits + `', '` + cash + `', 'COP', 100, '` + status + `', ` + expiresAt + `)`
	}
	asOwner(t, pool,
		`INSERT INTO ledger.accounts (id, legal_entity_id, code, kind, currency) VALUES ('`+cash+`', '`+entity+`', 'A.CASH', 'asset', 'COP')`,
		`INSERT INTO ledger.accounts (id, legal_entity_id, code, kind, currency) VALUES ('`+deposits+`', '`+entity+`', 'L.DEPOSITS', 'liability', 'COP')`,
		event(`'2026-03-01 10:00:00+00'`, `'2026-03-01 10:00:01+00'`), hold("expired", `'2026-03-01 10:00:00+00'`), hold("pending", `now() + interval '1 year'`))

	quiet, err := databaseState{pool: pool}.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if quiet.OldestOverdueHold.After(time.Now()) {
		t.Fatalf("the oldest overdue hold expires at %v, in the future", quiet.OldestOverdueHold)
	}

	asOwner(t, pool, event(`'2026-03-02 10:00:00+00'`, `NULL`), hold("pending", `'2026-03-02 11:00:00+00'`))
	s, err := databaseState{pool: pool}.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	emitted, expired := time.Date(2026, time.March, 2, 10, 0, 0, 0, time.UTC), time.Date(2026, time.March, 2, 11, 0, 0, 0, time.UTC)
	if !s.OldestUnpublished.Equal(emitted) || !s.OldestOverdueHold.Equal(expired) {
		t.Fatalf("oldest unpublished %v, oldest overdue hold %v; want %v and %v", s.OldestUnpublished, s.OldestOverdueHold, emitted, expired)
	}
}

// TestAnAsyncEntryWaitingIsReadAsTheApplication (Q-61): an entry of an async account past the aggregator's cursor,
// the only one waiting in this package's database, shows up, as the application's role reads it, as the oldest async
// entry waiting, with its transaction's time.
func TestAnAsyncEntryWaitingIsReadAsTheApplication(t *testing.T) {
	pool := appPool(t)
	entity, cash, fees, tx := ids.New().String(), ids.New().String(), ids.New().String(), ids.New().String()
	asOwner(t, pool,
		`INSERT INTO ledger.accounts (id, legal_entity_id, code, kind, currency) VALUES ('`+cash+`', '`+entity+`', 'A.CASH', 'asset', 'COP')`,
		`INSERT INTO ledger.accounts (id, legal_entity_id, code, kind, currency, balance_mode)
			VALUES ('`+fees+`', '`+entity+`', 'I.FEES', 'income', 'COP', 'async')`,
		`INSERT INTO ledger.transactions (id, legal_entity_id, code, value_date, business_date, source, external_ref, actor, channel, rule_version)
			VALUES ('`+tx+`', '`+entity+`', 'fee', current_date, current_date, 'metrics', '`+tx+`', 'tester', 'test', 'v1')`,
		`INSERT INTO ledger.entries (transaction_id, account_id, currency, amount)
			VALUES ('`+tx+`', '`+cash+`', 'COP', 100), ('`+tx+`', '`+fees+`', 'COP', -100)`)

	s, err := databaseState{pool: pool}.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var posted time.Time
	if err := pool.QueryRow(t.Context(), `SELECT posted_at FROM ledger.transactions WHERE id = $1`, tx).Scan(&posted); err != nil {
		t.Fatal(err)
	}
	if !s.OldestUnaggregated.Equal(posted) {
		t.Fatalf("the oldest async entry waiting was posted at %v, want %v, the one just posted", s.OldestUnaggregated, posted)
	}
}
