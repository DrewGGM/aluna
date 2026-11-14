//go:build integration

package httpapi

import (
	"net/http"
	"slices"
	"testing"

	"github.com/DrewGGM/aluna/internal/ids"
)

// expectReturnedToItsBank checks that transfer id is returned for reason, with its event, and that its return is
// queued for the rail once.
func expectReturnedToItsBank(t *testing.T, id, reason string) {
	t.Helper()
	var status, stored string
	if err := testPool.QueryRow(t.Context(), `SELECT status, reason FROM payments.orders WHERE id = $1`, id).Scan(&status, &stored); err != nil ||
		status != "returned" || stored != reason {
		t.Fatalf("the transfer is %s for %q (%v), want returned for %s", status, stored, err, reason)
	}
	if types := eventTypes(t, "payment-orders/"+id); !slices.Contains(types, "aluna.payments.order.returned.v1") {
		t.Fatalf("events = %v, want the return", types)
	}
	var jobs int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM infra.river_job WHERE kind = 'payments.return' AND args->>'order_id' = $1`,
		id).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("return jobs = %d (%v), want 1", jobs, err)
	}
}

// TestASuspenseIsReturnedWithAnotherOperator (FR-32, FR-39): a transfer in suspense that belongs to no customer is
// sent back to its bank once another operator approves it, not its maker: the order is returned for the reason
// given, the money leaves the suspense account, and the return is queued for the rail. A reason a rail cannot carry
// is 422 when requested, and an order that is not in suspense 409.
func TestASuspenseIsReturnedWithAnotherOperator(t *testing.T) {
	w, a := payingWorld(t)
	suspense := withSuspense(t, w)
	id := suspendedTransfer(t, w, newRailSide(t))

	expectProblem(t, "a blank reason", postTo(t, w.ts, "/v1/payment-orders/"+id+"/return-to-sender", ids.New().String(), `{"reason":"  "}`),
		http.StatusUnprocessableEntity, "invalid_payment_order")
	r := postTo(t, w.ts, "/v1/payment-orders/"+id+"/return-to-sender", ids.New().String(), `{"reason":"AC01"}`)
	requested := decodeApproval(t, r)
	if r.status != http.StatusAccepted || requested.Kind != "payments.return_to_sender" {
		t.Fatalf("request the return = %d %s, want 202 with a payments.return_to_sender request", r.status, r.body)
	}
	expectProblem(t, "the maker approving", postTo(t, w.ts, "/v1/approvals/"+requested.ID+"/approval", ids.New().String(), ""),
		http.StatusForbidden, "self_approval_forbidden")
	if got := approve(t, w.operatorB(t), requested.ID); got.Status != "approved" {
		t.Fatalf("approve = %+v, want approved", got)
	}

	expectReturnedToItsBank(t, id, "AC01")
	if held := balanceAt(t, w.ts, suspense, "").Natural.Amount; held != 0 {
		t.Fatalf("suspense = %d, want nothing left", held)
	}
	outgoing := paid(t, postTo(t, w.ts, "/v1/payment-orders", ids.New().String(), paymentBody(a, 30000000, "simbreb", "@beneficiary")))
	expectProblem(t, "return an outgoing payment", postTo(t, w.ts, "/v1/payment-orders/"+outgoing.Id.String()+"/return-to-sender",
		ids.New().String(), `{"reason":"AC01"}`), http.StatusConflict, "not_in_suspense")
}
