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
	return jobs.Deps{
		UnitOfWork: postgres.NewUnitOfWork(testPool), Ledger: ledger.New(calendar.New(), events), Idempotency: idempotency.NewStore(testPool), Outbox: events,
	}
}

// startWorker arranca un worker con la configuración de aluna worker (solo cambian el sondeo, el modo de
// prueba y el logger) y devuelve los jobs que termina.
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

// waitFor espera a que terminen bien los jobs de los tipos kinds; un job que falla detiene la prueba.
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
				t.Fatalf("el job %s falló: %v", ev.Job.Kind, ev.Job.Errors)
			}
			delete(pending, ev.Job.Kind)
		case <-timeout:
			t.Fatalf("no terminaron en 10 s: %v", pending)
		}
	}
}

func inTx(t *testing.T, fn func(tx postgres.Tx) error) {
	t.Helper()
	if err := postgres.NewUnitOfWork(testPool).Do(t.Context(), fn); err != nil {
		t.Fatal(err)
	}
}

// heldDeposit crea un depósito con 100 COP y le pone un hold de 30 que vence en expiresIn.
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
				LegalEntityID: entity, Code: "cta-" + ids.New().String(), Kind: kind, Currency: cop, Mode: ledger.BalanceModeSync,
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

// TestAHoldThatExpiredWhileTheWorkerWasDownExpiresOnStart: un hold vence con el worker detenido; al
// arrancar, la expiración corre enseguida, sin esperar su intervalo de 30 s.
func TestAHoldThatExpiredWhileTheWorkerWasDownExpiresOnStart(t *testing.T) {
	deps := newDeps(t)
	hold := heldDeposit(t, deps.Ledger, 500*time.Millisecond)
	time.Sleep(time.Until(hold.ExpiresAt()) + 100*time.Millisecond)

	waitFor(t, startWorker(t, deps), "ledger.holds.expire")

	if got := statusOf(t, hold.ID()); got != ledger.HoldExpired {
		t.Fatalf("hold %s, se esperaba %s", got, ledger.HoldExpired)
	}
}

// TestTheWorkerRunsEveryJob: al arrancar corren bien todos los periódicos, como aluna_app, y el worker
// publica lo que emite el outbox.
func TestTheWorkerRunsEveryJob(t *testing.T) {
	deps := newDeps(t)
	inTx(t, func(tx postgres.Tx) error {
		return deps.Outbox.Emit(t.Context(), tx, outbox.Event{
			Type: "aluna.test.worker.started.v1", Subject: "worker", LegalEntityID: ids.New(), SchemaVersion: 1, Data: map[string]any{},
		})
	})

	waitFor(t, startWorker(t, deps),
		"ledger.holds.expire", "ledger.balances.aggregate", "idempotency.purge", "outbox.purge", "outbox.publish")
}

// TestThePurgesHonorTheirRetention: la purga de idempotencia borra las claves de más de 7 días y la del
// outbox los eventos publicados hace más de 30; lo que está dentro de su retención queda.
func TestThePurgesHonorTheirRetention(t *testing.T) {
	deps := newDeps(t)
	keys := map[string]time.Duration{ids.New().String(): 8 * day, ids.New().String(): 6 * day}
	events := map[string]time.Duration{"worker/" + ids.New().String(): 31 * day, "worker/" + ids.New().String(): 29 * day}
	seedAged(t, deps, keys, events)

	waitFor(t, startWorker(t, deps), "idempotency.purge", "outbox.purge")

	for key, age := range keys {
		if n := count(t, `SELECT count(*) FROM infra.idempotency_keys WHERE key = $1`, key); n != boolInt(age < 7*day) {
			t.Errorf("clave de %v: quedan %d filas", age, n)
		}
	}
	for subject, age := range events {
		if n := count(t, `SELECT count(*) FROM infra.outbox WHERE subject = $1`, subject); n != boolInt(age < 30*day) {
			t.Errorf("evento publicado hace %v: quedan %d filas", age, n)
		}
	}
}

// seedAged crea claves de idempotencia completadas con la antigüedad de keys y eventos publicados hace lo
// que dice events. aluna_app no elige created_at ni published_at: los fija el dueño.
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
