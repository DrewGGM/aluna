//go:build integration

package httpapi

import (
	"net/http"
	"slices"
	"testing"

	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// returningPayment pays 300,007 COP from a, an amount the simulated rail accepts and then returns, and sends it:
// it returns the order and the rail's two notifications, accepted and returned, neither delivered yet.
func returningPayment(t *testing.T, w depositWorld, rails railSide, a gen.DepositAccount) (gen.PaymentOrder, notification, notification) {
	t.Helper()
	o := paid(t, postTo(t, w.ts, "/v1/payment-orders", ids.New().String(), paymentBody(a, 30000700, "simbreb", "@beneficiary")))
	if err := rails.payments.Send(t.Context(), postgres.NewUnitOfWork(testPool), ids.ID(o.Id)); err != nil {
		t.Fatal(err)
	}
	return o, rails.next(t), rails.next(t)
}

// delivered delivers n to the rails' server, which must take it (204).
func delivered(t *testing.T, rails railSide, what string, n notification) {
	t.Helper()
	if r := deliver(t, rails.ts, n); r.status != http.StatusNoContent {
		t.Fatalf("%s = %d %s, want 204", what, r.status, r.body)
	}
}

// TestAReturnedPaymentCreditsTheAccount (FR-31, TH-7): a settled payment the beneficiary's bank returns credits its
// amount back to the account from the rail's clearing account (its charges stay), the order is returned with the
// rail's reason, and the return or the acceptance delivered again change nothing.
func TestAReturnedPaymentCreditsTheAccount(t *testing.T) {
	w, a := payingWorld(t)
	withSuspense(t, w)
	rails := newRailSide(t)
	o, accepted, returned := returningPayment(t, w, rails, a)
	delivered(t, rails, "the acceptance", accepted)

	delivered(t, rails, "the return", returned)
	delivered(t, rails, "the return again", returned)
	delivered(t, rails, "the acceptance again", accepted)

	if now := readOrder(t, w.ts, o.Id.String()); now.Status != gen.PaymentOrderStatusReturned || now.Reason == nil || *now.Reason != "AC04" {
		t.Fatalf("the order = %+v, want it returned as AC04", now)
	}
	gmf := o.Held.Amount - o.Amount.Amount
	if balance := w.natural(t, a); balance != 100000000-gmf {
		t.Fatalf("balance = %d, want 1,000,000 COP less the payment's GMF (%d): the amount comes back once", balance, gmf)
	}
	if types := eventTypes(t, "payment-orders/"+o.Id.String()); !slices.Contains(types, "aluna.payments.order.returned.v1") {
		t.Fatalf("events = %v, want the return", types)
	}
}

// TestAReturnForAnInactiveAccountGoesToSuspense (FR-31, FR-32): if the account is no longer active, the returned
// amount goes to the rail's suspense account and the order is still returned.
func TestAReturnForAnInactiveAccountGoesToSuspense(t *testing.T) {
	w, a := payingWorld(t)
	suspense := withSuspense(t, w)
	rails := newRailSide(t)
	o, accepted, returned := returningPayment(t, w, rails, a)
	delivered(t, rails, "the acceptance", accepted)
	w.setStatus(t, a, deposits.Frozen)

	delivered(t, rails, "the return", returned)

	if now := readOrder(t, w.ts, o.Id.String()); now.Status != gen.PaymentOrderStatusReturned {
		t.Fatalf("the order = %+v, want it returned", now)
	}
	if balance, held := w.natural(t, a), balanceAt(t, w.ts, suspense, "").Natural.Amount; balance != 100000000-o.Held.Amount || held != o.Amount.Amount {
		t.Fatalf("balance = %d and suspense %d, want the account as the payment left it and the amount in suspense", balance, held)
	}
}

// TestAReturnBeforeTheAcceptanceConflicts (FR-31): the rail cannot return a payment it has not settled: a return
// that comes before the acceptance is 409, and the order is still sent until the acceptance comes.
func TestAReturnBeforeTheAcceptanceConflicts(t *testing.T) {
	w, a := payingWorld(t)
	rails := newRailSide(t)
	o, _, returned := returningPayment(t, w, rails, a)

	r := deliver(t, rails.ts, returned)

	expectProblem(t, "the return of a payment not settled", r, http.StatusConflict, "outcome_conflict")
	if now := readOrder(t, w.ts, o.Id.String()); now.Status != gen.PaymentOrderStatusSent {
		t.Fatalf("the order = %+v, want it still sent", now)
	}
}
