//go:build integration

package approvals_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/DrewGGM/aluna/internal/access"
	"github.com/DrewGGM/aluna/internal/approvals"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// TestEachRequestAndDecisionIsAnEvent (architecture §8): storing a request emits aluna.approvals.request.created.v1,
// and approving or rejecting it .approved.v1 or .rejected.v1, each in its transaction and about the request, with
// its kind and its status, never its payload nor who requested or decided it. A request whose command was rejected
// ends failed and emits no decision, and one that rolls back leaves no event.
func TestEachRequestAndDecisionIsAnEvent(t *testing.T) {
	rejecting := false
	w := newWorld(t, func(context.Context, postgres.Tx, json.RawMessage, access.Principal) (json.RawMessage, error) {
		if rejecting {
			return nil, fmt.Errorf("%w: insufficient funds", approvals.ErrCommandRejected)
		}
		return json.RawMessage(`{"posted": true}`), nil
	})
	payload := map[string]any{"account": "a business detail"}
	approved, rejected, failed := w.request(t, payload), w.request(t, payload), w.request(t, payload)
	rolledBack := w.requestAndRollBack(t, payload)

	if _, err := w.approve(t, approved.ID, w.checker); err != nil {
		t.Fatal(err)
	}
	if _, err := w.reject(t, rejected.ID, w.checker, "not applicable"); err != nil {
		t.Fatal(err)
	}
	rejecting = true
	if r, err := w.approve(t, failed.ID, w.checker); err != nil || r.Status != approvals.StatusFailed {
		t.Fatalf("approve a rejected command = %+v, %v; want it failed", r, err)
	}

	for _, c := range []struct {
		request ids.ID
		want    []pgtest.Event
	}{
		{approved.ID, []pgtest.Event{w.created(approved.ID), w.event(approved.ID, "aluna.approvals.request.approved.v1", "approved")}},
		{rejected.ID, []pgtest.Event{w.created(rejected.ID), w.event(rejected.ID, "aluna.approvals.request.rejected.v1", "rejected")}},
		{failed.ID, []pgtest.Event{w.created(failed.ID)}},
		{rolledBack, []pgtest.Event{}},
	} {
		if got := pgtest.EventsAbout(t, testPool, "approvals/"+c.request.String()); !reflect.DeepEqual(got, c.want) {
			t.Errorf("events of %s = %+v\nwant %+v", c.request, got, c.want)
		}
	}
}

// created is the event of the world's test request id once stored.
func (w world) created(id ids.ID) pgtest.Event {
	return w.event(id, "aluna.approvals.request.created.v1", "pending")
}

// event is the event of type typ of the world's test request id, which took status.
func (w world) event(id ids.ID, typ, status string) pgtest.Event {
	return pgtest.Event{
		Type: typ, LegalEntityID: w.maker.LegalEntityID, SchemaVersion: 1,
		Data: map[string]any{"request_id": id.String(), "kind": testKind, "status": status},
	}
}

// requestAndRollBack stores a request in a transaction that then rolls back, and returns its id.
func (w world) requestAndRollBack(t *testing.T, payload any) ids.ID {
	t.Helper()
	errRollback := errors.New("roll back")
	var id ids.ID
	err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		r, err := w.svc.Request(t.Context(), tx, testKind, payload, w.maker)
		if err != nil {
			return err
		}
		id = r.ID
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatalf("request and roll back = %v, want the rollback", err)
	}
	return id
}
