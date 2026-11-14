//go:build integration

package payments_test

import (
	"slices"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/payments"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// expireAt runs the expiry of pending orders as of at.
func (w world) expireAt(t *testing.T, at time.Time) {
	t.Helper()
	if _, err := w.payments.ExpirePending(t.Context(), postgres.NewUnitOfWork(testPool), at, 100); err != nil {
		t.Fatal(err)
	}
}

// TestTheExpiryTakesBatchesUntilItCatchesUp (FR-31): with batches of one, the expiry goes on until none is left.
func TestTheExpiryTakesBatchesUntilItCatchesUp(t *testing.T) {
	w := newWorld(t, true)
	var pending []payments.Order
	for range 3 {
		o, err := w.initiate(t, w.input(t, 30000000, "match-sanctioned"))
		if err != nil {
			t.Fatal(err)
		}
		pending = append(pending, o)
	}

	n, err := w.payments.ExpirePending(t.Context(), postgres.NewUnitOfWork(testPool), time.Now().Add(8*24*time.Hour), 1)

	if err != nil || n < len(pending) {
		t.Fatalf("expired %d (%v), want at least the world's %d", n, err, len(pending))
	}
	for _, o := range pending {
		if status := storedOrder(t, o.ID).status; status != "rejected" {
			t.Errorf("order %s is %s, want rejected", o.ID, status)
		}
	}
}

// reasonOf is the reason order o is stored with.
func (w world) reasonOf(t *testing.T, o payments.Order) string {
	t.Helper()
	now, err := w.payments.Order(t.Context(), testPool, w.entity, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	return now.Reason
}

// TestAPaymentThatWaitsTooLongExpires (FR-31, FR-42): an order still pending_screening 7 days after it was made is
// rejected as SCREENING_EXPIRED, with its event, and a younger one is not. Compliance resolving its case afterwards
// records the decision and leaves the order rejected: a customer who paid again does not pay twice.
func TestAPaymentThatWaitsTooLongExpires(t *testing.T) {
	w := newWorld(t, true)
	o, err := w.initiate(t, w.input(t, 30000000, "match-sanctioned"))
	if err != nil || o.Status != payments.StatusPendingScreening {
		t.Fatalf("pay a possible match = %+v (%v), want it pending_screening", o, err)
	}

	w.expireAt(t, time.Now().Add(6*24*time.Hour))
	if status := storedOrder(t, o.ID).status; status != "pending_screening" {
		t.Fatalf("after 6 days the order is %s, want it still pending_screening", status)
	}
	w.expireAt(t, time.Now().Add(8*24*time.Hour))

	if status, reason := storedOrder(t, o.ID).status, w.reasonOf(t, o); status != "rejected" || reason != "SCREENING_EXPIRED" {
		t.Fatalf("after 8 days the order is %s for %q, want rejected for SCREENING_EXPIRED", status, reason)
	}
	if events := eventsOf(t, o.ID); !slices.Contains(events, "aluna.payments.order.rejected.v1") {
		t.Fatalf("events = %v, want the rejection", events)
	}
	mustDo(t, func(tx postgres.Tx) error { return w.payments.ResolveScreening(t.Context(), tx, w.entity, o.ID, true) })
	if status := storedOrder(t, o.ID).status; status != "rejected" || sendJobs(t)[o.ID] {
		t.Fatalf("after clearing its case the order is %s (queued: %v), want it still rejected and not queued", status, sendJobs(t)[o.ID])
	}
}
