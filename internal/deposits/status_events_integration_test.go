//go:build integration

package deposits_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/party"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// TestEachStatusChangeIsAnEvent (architecture §8): every change of an account's status emits, in the same
// transaction, the event of that change about the account: activated (from pending or dormant), frozen,
// unfrozen, dormant and closed. A change whose transaction rolls back leaves no event, and a rejected one
// emits none.
func TestEachStatusChangeIsAnEvent(t *testing.T) {
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	w := newWorldWith(t, "2026-03-02", events)
	a, partyID := w.pendingAccount(t)
	inTx(t, func(tx postgres.Tx) error {
		_, err := w.parties.ChangeKYC(t.Context(), tx, w.entity, partyID, 1, party.KYCApproved)
		return err
	})
	w.activateAndRollBack(t, a.ID)

	for _, to := range []deposits.Status{deposits.Active, deposits.Frozen, deposits.Active, deposits.Dormant, deposits.Active, deposits.Closed} {
		if err := w.change(t, a.ID, to); err != nil {
			t.Fatalf("→ %s: %v", to, err)
		}
	}
	if err := w.change(t, a.ID, deposits.Active); !errors.Is(err, deposits.ErrInvalidTransition) {
		t.Fatalf("reopen = %v, want ErrInvalidTransition", err)
	}

	got, want := pgtest.EventsAbout(t, testPool, "deposit-accounts/"+a.ID.String()), lifeCycleEvents(w.entity, a.ID)
	if len(got) == 0 || got[0].Type != "aluna.deposits.account.opened.v1" {
		t.Fatalf("events of the account = %+v, want its opening first", got)
	}
	if !reflect.DeepEqual(got[1:], want) {
		t.Fatalf("status events = %+v\nwant %+v", got[1:], want)
	}
}

// activateAndRollBack activates account id in a transaction that then rolls back.
func (w world) activateAndRollBack(t *testing.T, id ids.ID) {
	t.Helper()
	errRollback := errors.New("roll back")
	err := try(t, func(tx postgres.Tx) error {
		if err := w.deposits.ChangeStatus(t.Context(), tx, w.entity, id, deposits.Active, "operator-a"); err != nil {
			return err
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatalf("activate and roll back = %v, want the rollback", err)
	}
}

// lifeCycleEvents are the events of account id of the entity going pending → active → frozen → active →
// dormant → active → closed on 2026-03-02.
func lifeCycleEvents(entity, id ids.ID) []pgtest.Event {
	changed := func(from, to deposits.Status) map[string]any {
		return map[string]any{"account_id": id.String(), "previous_status": string(from), "status": string(to)}
	}
	dormant := changed(deposits.Active, deposits.Dormant)
	dormant["last_customer_activity_on"] = "2026-03-02"
	events := []pgtest.Event{
		{Type: "aluna.deposits.account.activated.v1", Data: changed(deposits.Pending, deposits.Active)},
		{Type: "aluna.deposits.account.frozen.v1", Data: changed(deposits.Active, deposits.Frozen)},
		{Type: "aluna.deposits.account.unfrozen.v1", Data: changed(deposits.Frozen, deposits.Active)},
		{Type: "aluna.deposits.account.dormant.v1", Data: dormant},
		{Type: "aluna.deposits.account.activated.v1", Data: changed(deposits.Dormant, deposits.Active)},
		{Type: "aluna.deposits.account.closed.v1", Data: changed(deposits.Active, deposits.Closed)},
	}
	for i := range events {
		events[i].LegalEntityID, events[i].SchemaVersion = entity, 1
	}
	return events
}
