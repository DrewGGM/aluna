//go:build integration

package deposits_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// dormancyBatch is the batch of the tests' runs: small, so that a run takes several.
const dormancyBatch = 2

// dormantIn30 opens an active account of a savings product that turns dormant after 30 days without customer
// activity.
func (w world) dormantIn30(t *testing.T) deposits.Account {
	t.Helper()
	return w.openWith(t, map[string]string{"dormancy_days": "30"})
}

// toApril2 sets the world's business date to Thursday 2026-04-02, 31 days after its first, as the tables'
// owner: weeks of rollovers are not what the tests are about.
func (w world) toApril2(t *testing.T) {
	t.Helper()
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE calendar.legal_entities SET business_date = '2026-04-02' WHERE id = $1`, w.entity)
		return err
	})
}

func (w world) markDormant(t *testing.T) int {
	t.Helper()
	n, err := w.deposits.MarkDormant(t.Context(), postgres.NewUnitOfWork(testPool), dormancyBatch)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// dormantEvents are the payloads of the world's dormant events.
func (w world) dormantEvents(t *testing.T) []string {
	t.Helper()
	var got []string
	for _, e := range w.events.events {
		if e.Type != "aluna.deposits.account.dormant.v1" {
			continue
		}
		data, err := json.Marshal(e.Data)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, e.Subject+" "+string(data))
	}
	return got
}

// TestAnInactiveAccountTurnsDormant (FR-27): an active account whose customer has not moved it for more than
// its product's days turns dormant, with its change recorded and its event; one inactive for exactly those
// days does not, nor does an inactive account that is not active (of the same product), nor one as inactive
// whose product allows a year.
func TestAnInactiveAccountTurnsDormant(t *testing.T) {
	w := newWorld(t)
	w.vault(t)
	in30 := w.productWith(t, map[string]string{"dormancy_days": "30"})
	quiet, recent, frozen, yearly := w.openOf(t, in30), w.openOf(t, in30), w.openOf(t, in30), w.active(t)
	if err := w.change(t, frozen.ID, deposits.Frozen); err != nil {
		t.Fatal(err)
	}
	w.rollover(t, march(2))
	if err := w.deposit(t, recent.ID, 100); err != nil { // its last activity becomes March 3
		t.Fatal(err)
	}
	w.toApril2(t) // 31 days after March 2, 30 after March 3

	w.markDormant(t)

	for _, c := range []struct {
		name string
		a    deposits.Account
		want deposits.Status
	}{
		{"31 days without activity", quiet, deposits.Dormant},
		{"30 days without activity", recent, deposits.Active},
		{"frozen", frozen, deposits.Frozen},
		{"31 days, a product of a year", yearly, deposits.Active},
	} {
		if got := w.reread(t, c.a).Status; got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	if n := count(t, `SELECT count(*) FROM deposits.status_changes WHERE account_id = $1 AND from_status = 'active'
		AND to_status = 'dormant' AND actor = 'deposits.dormancy'`, quiet.ID); n != 1 {
		t.Fatalf("%d recorded changes to dormant, want 1 by the dormancy job", n)
	}
	want := "deposit-accounts/" + quiet.ID.String() + ` {"account_id":"` + quiet.ID.String() + `","last_customer_activity_on":"2026-03-02"}`
	if got := w.dormantEvents(t); len(got) != 1 || got[0] != want {
		t.Fatalf("dormant events = %v, want once %s", got, want)
	}
}

// TestMarkingDormantTwiceChangesNothing (FR-27): a second run finds no active account to mark, records no
// change and emits nothing.
func TestMarkingDormantTwiceChangesNothing(t *testing.T) {
	w := newWorld(t)
	w.vault(t)
	a := w.dormantIn30(t)
	w.toApril2(t)
	w.markDormant(t)

	again := w.markDormant(t)

	changes := count(t, `SELECT count(*) FROM deposits.status_changes WHERE account_id = $1 AND to_status = 'dormant'`, a.ID)
	if again != 0 || changes != 1 || len(w.dormantEvents(t)) != 1 {
		t.Fatalf("second run marked %d; %d changes, %d events; want 0, 1 and 1", again, changes, len(w.dormantEvents(t)))
	}
}

// TestInterestIsNotCustomerActivity (FR-27): a capitalization credits the account without being its
// customer's doing, so the account still turns dormant counting from its last deposit.
func TestInterestIsNotCustomerActivity(t *testing.T) {
	w := newWorld(t)
	w.vault(t)
	a := w.dormantIn30(t) // following: March 2 capitalizes February
	if err := w.deposit(t, a.ID, 100000000); err != nil {
		t.Fatal(err)
	}
	w.rollover(t, march(2))
	w.accrue(t)
	if _, ok := w.capitalization(t, a, "2026-02"); !ok {
		t.Fatal("no capitalization on March 2")
	}
	w.toApril2(t)

	w.markDormant(t)

	if got := w.reread(t, a); got.Status != deposits.Dormant || got.LastCustomerActivityOn != march(2) {
		t.Fatalf("account = %+v, want dormant with its last activity on March 2", got)
	}
}

// TestADormantAccountComesBackWithMoney: a dormant account turns active again through a status change, and
// then moves money (the reactivation by a customer movement is not automatic yet: Q-110 c).
func TestADormantAccountComesBackWithMoney(t *testing.T) {
	w := newWorld(t)
	w.vault(t)
	a := w.dormantIn30(t)
	w.toApril2(t)
	w.markDormant(t)

	blocked := w.deposit(t, a.ID, 100)
	if err := w.change(t, a.ID, deposits.Active); err != nil {
		t.Fatal(err)
	}
	reactivated := w.deposit(t, a.ID, 100)

	if !errors.Is(blocked, deposits.ErrAccountNotActive) || reactivated != nil {
		t.Fatalf("deposit while dormant = %v, once active = %v; want ErrAccountNotActive, then nil", blocked, reactivated)
	}
}

// TestAReactivatedAccountStartsItsDaysAgain (FR-27): moving an account to active, from dormant or after a
// freeze longer than its days, records the business date as its last customer activity, so the next run of
// the job does not take it back to dormant before its customer could move it.
func TestAReactivatedAccountStartsItsDaysAgain(t *testing.T) {
	w := newWorld(t)
	w.vault(t)
	dormant, frozen := w.dormantIn30(t), w.dormantIn30(t)
	if err := w.change(t, frozen.ID, deposits.Frozen); err != nil {
		t.Fatal(err)
	}
	w.toApril2(t)
	w.markDormant(t)
	for _, a := range []deposits.Account{dormant, frozen} {
		if err := w.change(t, a.ID, deposits.Active); err != nil {
			t.Fatal(err)
		}
	}

	w.markDormant(t)

	for name, a := range map[string]deposits.Account{"reactivated": dormant, "unfrozen": frozen} {
		if got := w.reread(t, a); got.Status != deposits.Active || got.LastCustomerActivityOn != (calendar.Date{Year: 2026, Month: time.April, Day: 2}) {
			t.Errorf("%s account = %+v, want it active with its last activity on April 2", name, got)
		}
	}
}

// TestALockedAccountTurnsDormantLater: an account locked by a movement in progress is skipped, without
// waiting for it, and the next run marks it.
func TestALockedAccountTurnsDormantLater(t *testing.T) {
	w := newWorld(t)
	w.vault(t)
	busy, free := w.dormantIn30(t), w.dormantIn30(t)
	w.toApril2(t)
	_, finish := holdOpen(t, func(tx postgres.Tx) error {
		_, err := tx.Exec(t.Context(), `SELECT 1 FROM deposits.accounts WHERE id = $1 FOR UPDATE`, busy.ID)
		return err
	})

	w.markDormant(t)
	whileBusy, freeStatus := w.reread(t, busy).Status, w.reread(t, free).Status
	if err := finish(); err != nil {
		t.Fatal(err)
	}
	w.markDormant(t)

	if whileBusy != deposits.Active || freeStatus != deposits.Dormant || w.reread(t, busy).Status != deposits.Dormant {
		t.Fatalf("while locked: %s (free %s); after: %s; want active (dormant), then dormant", whileBusy, freeStatus, w.reread(t, busy).Status)
	}
}
