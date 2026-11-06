//go:build integration

package compliance_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/DrewGGM/aluna/internal/compliance"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

const posted = "aluna.ledger.transaction.posted.v1"

// cashWorld is an entity with the accounts of two customers, each held by a party, and its cash account.
type cashWorld struct {
	entity, holder, customer, neighbor, neighborAccount, vault ids.ID
}

func newCashWorld() cashWorld {
	return cashWorld{entity: ids.New(), holder: ids.New(), customer: ids.New(), neighbor: ids.New(), neighborAccount: ids.New(), vault: ids.New()}
}

// holderOf is deposits as the cash totals see it: only the customers' accounts have a holder.
func (w cashWorld) holderOf(_ context.Context, _ postgres.Tx, entity, account ids.ID) (compliance.Holder, bool, error) {
	switch {
	case entity != w.entity:
		return compliance.Holder{}, false, nil
	case account == w.customer:
		return compliance.Holder{PartyID: w.holder, CashAccount: w.vault}, true, nil
	case account == w.neighborAccount:
		return compliance.Holder{PartyID: w.neighbor, CashAccount: w.vault}, true, nil
	}
	return compliance.Holder{}, false, nil
}

type entry struct {
	account ids.ID
	units   int64
}

// transaction is the data of a posted transaction on businessDate, in currency, with entries.
func transaction(channel, businessDate, currency string, entries ...entry) map[string]any {
	list := make([]map[string]any, len(entries))
	for i, e := range entries {
		list[i] = map[string]any{"account_id": e.account, "amount": map[string]any{"amount": e.units, "currency": currency}}
	}
	return map[string]any{"channel": channel, "business_date": businessDate, "entries": list}
}

// take hands the data to the consumer, as the outbox would, and reports what it answers.
func (w cashWorld) take(t *testing.T, data any) error {
	t.Helper()
	body, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	handle := compliance.AccumulatingCash(w.holderOf)
	return postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		return handle(t.Context(), tx, outbox.StoredEvent{ID: ids.New(), Type: posted, LegalEntityID: w.entity, Data: body})
	})
}

// took is take, which must succeed.
func (w cashWorld) took(t *testing.T, data any) {
	t.Helper()
	if err := w.take(t, data); err != nil {
		t.Fatal(err)
	}
}

// total is the total of the party in the month, direction and currency; 0 if there is none.
func (w cashWorld) total(t *testing.T, party ids.ID, month, direction, currency string) int64 {
	t.Helper()
	var units int64
	err := testPool.QueryRow(t.Context(), `SELECT total_units FROM compliance.cash_totals
		WHERE legal_entity_id = $1 AND party_id = $2 AND month = $3 AND direction = $4 AND currency = $5`,
		w.entity, party, month, direction, currency).Scan(&units)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return units
}

func (w cashWorld) rows(t *testing.T) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM compliance.cash_totals WHERE legal_entity_id = $1`, w.entity).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestCashAccumulatesByPartyMonthAndDirection (FR-43): deposits and withdrawals add up apart, within a party, a
// month and a currency.
func TestCashAccumulatesByPartyMonthAndDirection(t *testing.T) {
	w := newCashWorld()
	w.took(t, transaction("cash", "2026-03-02", "COP", entry{w.vault, 100_00}, entry{w.customer, -100_00}))
	w.took(t, transaction("cash", "2026-03-31", "COP", entry{w.vault, 50_00}, entry{w.customer, -50_00}))
	w.took(t, transaction("cash", "2026-04-01", "COP", entry{w.vault, 7_00}, entry{w.customer, -7_00}))
	w.took(t, transaction("cash", "2026-03-15", "COP", entry{w.customer, 30_00}, entry{w.vault, -30_00}))
	w.took(t, transaction("cash", "2026-03-15", "USD", entry{w.vault, 9_00}, entry{w.customer, -9_00}))
	w.took(t, transaction("cash", "2026-03-20", "COP", entry{w.vault, 3_00}, entry{w.neighborAccount, -3_00}))

	for _, c := range []struct {
		party                      ids.ID
		month, direction, currency string
		want                       int64
	}{
		{w.holder, "2026-03-01", "deposit", "COP", 150_00},
		{w.holder, "2026-04-01", "deposit", "COP", 7_00},
		{w.holder, "2026-03-01", "withdrawal", "COP", 30_00},
		{w.holder, "2026-03-01", "deposit", "USD", 9_00},
		{w.neighbor, "2026-03-01", "deposit", "COP", 3_00},
	} {
		if got := w.total(t, c.party, c.month, c.direction, c.currency); got != c.want {
			t.Errorf("%s %s %s %s = %d, want %d", c.party, c.month, c.direction, c.currency, got, c.want)
		}
	}
	if n := w.rows(t); n != 5 {
		t.Errorf("%d totals, want 5", n)
	}
}

// TestChargesAreNotCash: a withdrawal counts the cash handed over, not what the account paid with its fee and
// GMF, wherever they come in the transaction; a deposit likewise.
func TestChargesAreNotCash(t *testing.T) {
	w := newCashWorld()
	fees, gmf := ids.New(), ids.New()
	w.took(t, transaction("cash", "2026-03-02", "COP",
		entry{w.customer, 5_00}, entry{fees, -5_00}, entry{w.customer, 1_00}, entry{gmf, -1_00}, entry{w.customer, 100_00}, entry{w.vault, -100_00}))
	w.took(t, transaction("cash", "2026-03-02", "COP",
		entry{w.vault, 70_00}, entry{w.customer, -70_00}, entry{w.customer, 2_00}, entry{fees, -2_00}))

	if got := w.total(t, w.holder, "2026-03-01", "withdrawal", "COP"); got != 100_00 {
		t.Errorf("withdrawn %d, want 100_00", got)
	}
	if got := w.total(t, w.holder, "2026-03-01", "deposit", "COP"); got != 70_00 {
		t.Errorf("deposited %d, want 70_00", got)
	}
}

// TestSeveralEntriesOnTheCashAccountAddUp: the cash account can come in more than one entry.
func TestSeveralEntriesOnTheCashAccountAddUp(t *testing.T) {
	w := newCashWorld()
	w.took(t, transaction("cash", "2026-03-02", "COP", entry{w.vault, 60_00}, entry{w.vault, 40_00}, entry{w.customer, -100_00}))
	if got := w.total(t, w.holder, "2026-03-01", "deposit", "COP"); got != 100_00 {
		t.Errorf("deposited %d, want 100_00", got)
	}
}

// TestOnlyCashOfAPartyCounts: another channel, a movement between the entity's own accounts and a transaction
// that does not touch the cash account add nothing.
func TestOnlyCashOfAPartyCounts(t *testing.T) {
	w := newCashWorld()
	other := ids.New()
	w.took(t, transaction("api", "2026-03-02", "COP", entry{w.vault, 100_00}, entry{w.customer, -100_00}))
	w.took(t, transaction("rail", "2026-03-02", "COP", entry{w.vault, 100_00}, entry{w.customer, -100_00}))
	w.took(t, transaction("cash", "2026-03-02", "COP", entry{w.vault, 100_00}, entry{other, -100_00}))
	w.took(t, transaction("cash", "2026-03-02", "COP", entry{other, 100_00}, entry{w.customer, -100_00}))

	if n := w.rows(t); n != 0 {
		t.Errorf("%d totals, want none", n)
	}
}

// TestAnEventThatCannotBeReadFails: it stays for the job to retry instead of being dropped.
func TestAnEventThatCannotBeReadFails(t *testing.T) {
	w := newCashWorld()
	for name, data := range map[string]any{
		"not a transaction":   []string{"a list"},
		"no business date":    map[string]any{"channel": "cash"},
		"an unknown currency": transaction("cash", "2026-03-02", "XXQ", entry{w.vault, 1}),
	} {
		if err := w.take(t, data); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// republish is the job that publishes an event, as the outbox inserts it.
type republish struct {
	EventID ids.ID `json:"event_id"`
}

func (republish) Kind() string { return "outbox.publish" }

// TestAnEventDeliveredTwiceAddsOnce: the publication job is at-least-once; the same event delivered again adds
// nothing.
func TestAnEventDeliveredTwiceAddsOnce(t *testing.T) {
	w := newCashWorld()
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	compliance.FollowCash(events, w.holderOf)
	workers := river.NewWorkers()
	events.AddWorkers(workers)
	client, err := river.NewClient(riverpgxv5.New(testPool), &river.Config{
		Schema: outbox.RiverSchema, Workers: workers, Queues: map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}},
		FetchPollInterval: 100 * time.Millisecond, TestOnly: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	finished, stop := client.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
	defer stop()
	if err := client.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Error(err)
		}
	})

	id := ids.New()
	err = postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		return events.Emit(t.Context(), tx, outbox.Event{
			ID: id, Type: posted, Subject: "transactions/" + ids.New().String(), LegalEntityID: w.entity, SchemaVersion: 1,
			Data: transaction("cash", "2026-03-02", "COP", entry{w.vault, 100_00}, entry{w.customer, -100_00}),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForJob(t, finished)
	if _, err := client.Insert(t.Context(), republish{EventID: id}, nil); err != nil {
		t.Fatal(err)
	}
	waitForJob(t, finished)

	if got := w.total(t, w.holder, "2026-03-01", "deposit", "COP"); got != 100_00 {
		t.Errorf("total after two deliveries = %d, want 100_00", got)
	}
}

// waitForJob waits for one job to succeed; a failing job stops the test.
func waitForJob(t *testing.T, finished <-chan *river.Event) {
	t.Helper()
	select {
	case ev := <-finished:
		if ev.Kind == river.EventKindJobFailed {
			t.Fatalf("job %s failed: %v", ev.Job.Kind, ev.Job.Errors)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no job finished within 10 s")
	}
}

// TestACashTotalOnlyGrows: a total goes down, moves to another party or disappears in no way, for the
// application's role or for the owner.
func TestACashTotalOnlyGrows(t *testing.T) {
	w := newCashWorld()
	w.took(t, transaction("cash", "2026-03-02", "COP", entry{w.vault, 100_00}, entry{w.customer, -100_00}))
	const (
		lower = `UPDATE compliance.cash_totals SET total_units = 1 WHERE legal_entity_id = $1`
		move  = `UPDATE compliance.cash_totals SET party_id = gen_random_uuid() WHERE legal_entity_id = $1`
		drop  = `DELETE FROM compliance.cash_totals WHERE legal_entity_id = $1`
		wipe  = `TRUNCATE compliance.cash_totals`
	)

	for statement, want := range map[string]string{lower: "only grows", move: "permission denied", drop: "permission denied"} {
		_, err := testPool.Exec(t.Context(), statement, w.entity)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", statement, err, want)
		}
	}
	for _, statement := range []string{move, drop, wipe} {
		pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(t.Context(), `SAVEPOINT guarded`); err != nil {
				return err
			}
			args := []any{w.entity}
			if statement == wipe {
				args = nil
			}
			_, err := tx.Exec(t.Context(), statement, args...)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "2F003" {
				t.Errorf("%s as the owner: %v, want 2F003", statement, err)
			}
			_, err = tx.Exec(t.Context(), `ROLLBACK TO SAVEPOINT guarded`)
			return err
		})
	}
}
