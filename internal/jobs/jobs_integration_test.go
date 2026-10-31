//go:build integration

package jobs_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/DrewGGM/aluna/internal/audit"
	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/jobs"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, &testPool)) }

func newDeps(t *testing.T) jobs.Deps {
	t.Helper()
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	records, err := audit.WithFileAnchor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return jobs.Deps{
		Audit:      records,
		UnitOfWork: postgres.NewUnitOfWork(testPool), Ledger: ledger.New(calendar.New(), events), Idempotency: idempotency.NewStore(testPool), Outbox: events,
	}
}

// startWorker starts a worker with the aluna worker configuration (only the polling, the test mode and
// the logger change) and returns the jobs it finishes.
func startWorker(t *testing.T, deps jobs.Deps) <-chan *river.Event {
	t.Helper()
	cfg := jobs.Config(deps)
	cfg.FetchPollInterval, cfg.TestOnly, cfg.Logger = 100*time.Millisecond, true, slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := river.NewClient(riverpgxv5.New(testPool), cfg)
	if err != nil {
		t.Fatal(err)
	}
	finished, _ := client.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
	if err := client.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Error(err)
		}
	})
	return finished
}

// waitFor waits for the jobs of the given kinds to succeed; a failing job stops the test.
func waitFor(t *testing.T, finished <-chan *river.Event, kinds ...string) {
	t.Helper()
	pending := map[string]bool{}
	for _, k := range kinds {
		pending[k] = true
	}
	timeout := time.After(10 * time.Second)
	for len(pending) > 0 {
		select {
		case ev := <-finished:
			if ev.Kind == river.EventKindJobFailed {
				t.Fatalf("job %s failed: %v", ev.Job.Kind, ev.Job.Errors)
			}
			delete(pending, ev.Job.Kind)
		case <-timeout:
			t.Fatalf("did not finish within 10 s: %v", pending)
		}
	}
}

func inTx(t *testing.T, fn func(tx postgres.Tx) error) {
	t.Helper()
	if err := postgres.NewUnitOfWork(testPool).Do(t.Context(), fn); err != nil {
		t.Fatal(err)
	}
}

// heldDeposit creates a deposit with 100 COP and places a hold of 30 on it that expires in expiresIn.
func heldDeposit(t *testing.T, svc *ledger.Service, expiresIn time.Duration) ledger.Hold {
	t.Helper()
	cop, err := money.CurrencyOf("COP")
	if err != nil {
		t.Fatal(err)
	}
	entity, today := ids.New(), time.Now().UTC().Truncate(24*time.Hour)
	var hold ledger.Hold
	inTx(t, func(tx postgres.Tx) error {
		var accounts [3]ledger.Account
		for i, kind := range []ledger.Kind{ledger.KindAsset, ledger.KindLiability, ledger.KindLiability} {
			if accounts[i], err = svc.CreateAccount(t.Context(), tx, ledger.CreateAccountInput{
				LegalEntityID: entity, Code: "acct-" + ids.New().String(), Kind: kind, Currency: cop, Mode: ledger.BalanceModeSync,
			}); err != nil {
				return err
			}
		}
		cash, deposit, clearing := accounts[0], accounts[1], accounts[2]
		if _, err := svc.Post(t.Context(), tx, ledger.PostInput{
			LegalEntityID: entity, Code: "test", ValueDate: today, BusinessDate: today, Source: "test", ExternalRef: ids.New().String(),
			Actor: "tester", Channel: "test", RuleVersion: "v1",
			Entries: []ledger.EntryInput{{AccountID: cash.ID, Amount: money.New(100, cop)}, {AccountID: deposit.ID, Amount: money.New(-100, cop)}},
		}); err != nil {
			return err
		}
		hold, err = svc.PlaceHold(t.Context(), tx, ledger.PlaceHoldInput{
			LegalEntityID: entity, AccountID: deposit.ID, CounterAccountID: clearing.ID,
			Amount: money.New(30, cop), ExpiresAt: time.Now().Add(expiresIn),
		})
		return err
	})
	return hold
}

func statusOf(t *testing.T, hold ids.ID) ledger.HoldStatus {
	t.Helper()
	var status ledger.HoldStatus
	if err := testPool.QueryRow(t.Context(), `SELECT status FROM ledger.holds WHERE id = $1`, hold).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// TestAHoldThatExpiredWhileTheWorkerWasDownExpiresOnStart: a hold expires while the worker is stopped;
// on startup, expiration runs right away, without waiting for its 30 s interval.
func TestAHoldThatExpiredWhileTheWorkerWasDownExpiresOnStart(t *testing.T) {
	deps := newDeps(t)
	hold := heldDeposit(t, deps.Ledger, 500*time.Millisecond)
	time.Sleep(time.Until(hold.ExpiresAt()) + 100*time.Millisecond)

	waitFor(t, startWorker(t, deps), "ledger.holds.expire")

	if got := statusOf(t, hold.ID()); got != ledger.HoldExpired {
		t.Fatalf("hold %s, want %s", got, ledger.HoldExpired)
	}
}

// TestTheWorkerRunsEveryJob: on startup every periodic job succeeds, as aluna_app: the worker publishes
// what the outbox emits and seals what is in the audit log.
func TestTheWorkerRunsEveryJob(t *testing.T) {
	deps := newDeps(t)
	correlation := ids.New().String()
	inTx(t, func(tx postgres.Tx) error {
		if err := audit.New().Record(t.Context(), tx, audit.Record{
			Actor: "api", Role: "api", Channel: "api", CorrelationID: correlation, Command: "POST /v1/transactions", Result: "ok",
		}); err != nil {
			return err
		}
		return deps.Outbox.Emit(t.Context(), tx, outbox.Event{
			Type: "aluna.test.worker.started.v1", Subject: "worker", LegalEntityID: ids.New(), SchemaVersion: 1, Data: map[string]any{},
		})
	})

	waitFor(t, startWorker(t, deps),
		"ledger.holds.expire", "ledger.balances.aggregate", "idempotency.purge", "outbox.purge", "outbox.publish", "audit.seal")

	var block *int64
	if err := testPool.QueryRow(t.Context(), `SELECT block_id FROM audit.records WHERE correlation_id = $1`, correlation).Scan(&block); err != nil || block == nil {
		t.Fatalf("the audit record was left unsealed (%v)", err)
	}
	if err := deps.Audit.Verify(t.Context(), testPool, *block, *block); err != nil {
		t.Fatalf("the block the worker sealed does not verify: %v", err)
	}
}

// TestThePurgesHonorTheirRetention: the idempotency purge deletes keys older than 7 days and the outbox
// purge events published more than 30 days ago; whatever is within its retention stays.
func TestThePurgesHonorTheirRetention(t *testing.T) {
	deps := newDeps(t)
	keys := map[string]time.Duration{ids.New().String(): 8 * day, ids.New().String(): 6 * day}
	events := map[string]time.Duration{"worker/" + ids.New().String(): 31 * day, "worker/" + ids.New().String(): 29 * day}
	seedAged(t, deps, keys, events)

	waitFor(t, startWorker(t, deps), "idempotency.purge", "outbox.purge")

	for key, age := range keys {
		if n := count(t, `SELECT count(*) FROM infra.idempotency_keys WHERE key = $1`, key); n != boolInt(age < 7*day) {
			t.Errorf("key aged %v: %d rows left", age, n)
		}
	}
	for subject, age := range events {
		if n := count(t, `SELECT count(*) FROM infra.outbox WHERE subject = $1`, subject); n != boolInt(age < 30*day) {
			t.Errorf("event published %v ago: %d rows left", age, n)
		}
	}
}

// seedAged creates completed idempotency keys with the ages in keys and events published as long ago as
// events says. aluna_app does not choose created_at or published_at: the owner sets them.
func seedAged(t *testing.T, deps jobs.Deps, keys, events map[string]time.Duration) {
	t.Helper()
	inTx(t, func(tx postgres.Tx) error {
		for subject := range events {
			if err := deps.Outbox.Emit(t.Context(), tx, outbox.Event{
				Type: "aluna.test.worker.started.v1", Subject: subject, LegalEntityID: ids.New(), SchemaVersion: 1, Data: map[string]any{},
			}); err != nil {
				return err
			}
		}
		return nil
	})
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		for key, age := range keys {
			if _, err := tx.Exec(t.Context(), `INSERT INTO infra.idempotency_keys (scope, key, request_hash, status, response_code, response_body, created_at)
				VALUES ('test', $1, $3, 'completed', 201, '{}', now() - $2::interval)`, key, age, make([]byte, 32)); err != nil {
				return err
			}
		}
		for subject, age := range events {
			if _, err := tx.Exec(t.Context(), `UPDATE infra.outbox SET published_at = now() - $2::interval WHERE subject = $1`, subject, age); err != nil {
				return err
			}
		}
		return nil
	})
}

func count(t *testing.T, query string, arg any) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(t.Context(), query, arg).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

const day = 24 * time.Hour

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
