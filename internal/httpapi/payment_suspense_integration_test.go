//go:build integration

package httpapi

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// suspendedTransfer has the rail deliver a transfer of 300,000 COP for no account of the world's entity, which goes
// to the rail's suspense account, and returns its order's id.
func suspendedTransfer(t *testing.T, w depositWorld, rails railSide) string {
	t.Helper()
	e2e := newE2E()
	r := deliverTo(t, rails.ts, "transfers", signedTransfer(receivedBody(w.entity, e2e, 30000000, "no-such-account")))
	if r.status != http.StatusNoContent {
		t.Fatalf("the transfer = %d %s, want 204", r.status, r.body)
	}
	var id string
	if err := testPool.QueryRow(t.Context(), `SELECT id FROM payments.orders WHERE end_to_end_id = $1 AND status = 'suspended'`, e2e).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// creditRequested asks, as the world's operator, to credit transfer id to account a: the request must be pending.
func (w depositWorld) creditRequested(t *testing.T, id string, a gen.DepositAccount) testApproval {
	t.Helper()
	r := postTo(t, w.ts, "/v1/payment-orders/"+id+"/credit", ids.New().String(), `{"account_id":"`+a.Id.String()+`"}`)
	got := decodeApproval(t, r)
	if r.status != http.StatusAccepted || got.Status != "pending" || got.Kind != "payments.suspense_credit" {
		t.Fatalf("request the credit = %d %s, want 202 with a pending payments.suspense_credit request", r.status, r.body)
	}
	return got
}

// orderStatus is where order id stands and the account it is for.
func orderStatus(t *testing.T, id string) (string, *ids.ID) {
	t.Helper()
	var status string
	var account *ids.ID
	if err := testPool.QueryRow(t.Context(), `SELECT status, account_id FROM payments.orders WHERE id = $1`, id).Scan(&status, &account); err != nil {
		t.Fatal(err)
	}
	return status, account
}

// TestASuspenseIsCreditedWithAnotherOperator (FR-32, FR-39): a transfer in suspense is credited to the account
// someone found for it once another operator approves it, not its maker: the amount moves from the suspense
// account to the account, and the order settles for it, with its event.
func TestASuspenseIsCreditedWithAnotherOperator(t *testing.T) {
	w, a := payingWorld(t)
	suspense := withSuspense(t, w)
	rails := newRailSide(t)
	id := suspendedTransfer(t, w, rails)

	credit := w.creditRequested(t, id, a)
	expectEvidence(t, credit)
	expectProblem(t, "the maker approving", postTo(t, w.ts, "/v1/approvals/"+credit.ID+"/approval", ids.New().String(), ""),
		http.StatusForbidden, "self_approval_forbidden")
	if got := approve(t, w.operatorB(t), credit.ID); got.Status != "approved" {
		t.Fatalf("approve = %+v, want approved", got)
	}

	if balance, held := w.natural(t, a), balanceAt(t, w.ts, suspense, "").Natural.Amount; balance != 130000000 || held != 0 {
		t.Fatalf("balance = %d and suspense %d, want 1,300,000 COP and nothing", balance, held)
	}
	if status, account := orderStatus(t, id); status != "settled" || account == nil || *account != ids.ID(a.Id) {
		t.Fatalf("the order is %s for %v, want settled for the account", status, account)
	}
	if types := eventTypes(t, "payment-orders/"+id); !slices.Contains(types, "aluna.payments.order.settled.v1") {
		t.Fatalf("events = %v, want the order settled", types)
	}
	expectPostedBy(t, id+"/credit", "operator-b")
}

// expectPostedBy checks that the payments transaction of reference ref was posted by actor through the API.
func expectPostedBy(t *testing.T, ref, actor string) {
	t.Helper()
	var id ids.ID
	if err := testPool.QueryRow(t.Context(), `SELECT id FROM ledger.transactions WHERE source = 'payments' AND external_ref = $1`,
		ref).Scan(&id); err != nil {
		t.Fatalf("payments transaction %s: %v", ref, err)
	}
	expectPostedThroughAPI(t, id, actor)
}

// expectEvidence checks that credit request r shows its checker what the transfer is: its amount, its end-to-end id
// and why it went to suspense.
func expectEvidence(t *testing.T, r testApproval) {
	t.Helper()
	var evidence suspenseCredit
	if err := json.Unmarshal(r.Payload, &evidence); err != nil || evidence.Amount.Amount != 30000000 || evidence.Reason == "" || evidence.EndToEndID == "" {
		t.Fatalf("payload = %s (%v), want the transfer's amount, end-to-end id and reason for the checker", r.Payload, err)
	}
}

// eventTypes are the types of the events about subject.
func eventTypes(t *testing.T, subject string) []string {
	t.Helper()
	var types []string
	for _, e := range pgtest.EventsAbout(t, testPool, subject) {
		types = append(types, e.Type)
	}
	return types
}

// TestOnlyASuspenseIsCredited (FR-32, FR-41): an order that is not a transfer in suspense is 409 not_in_suspense,
// and an unknown one, or another entity's suspense, 404; none leaves a request.
func TestOnlyASuspenseIsCredited(t *testing.T) {
	w, a := payingWorld(t)
	withSuspense(t, w)
	suspended := suspendedTransfer(t, w, newRailSide(t))
	outgoing := paid(t, postTo(t, w.ts, "/v1/payment-orders", ids.New().String(), paymentBody(a, 30000000, "simbreb", "@beneficiary")))
	body := `{"account_id":"` + a.Id.String() + `"}`

	expectProblem(t, "credit an outgoing payment", postTo(t, w.ts, "/v1/payment-orders/"+outgoing.Id.String()+"/credit", ids.New().String(), body),
		http.StatusConflict, "not_in_suspense")
	expectProblem(t, "credit an unknown order", postTo(t, w.ts, "/v1/payment-orders/"+ids.New().String()+"/credit", ids.New().String(), body),
		http.StatusNotFound, "not_found")
	expectProblem(t, "credit another entity's suspense", postTo(t, apiServer(t, ids.New()), "/v1/payment-orders/"+suspended+"/credit",
		ids.New().String(), body), http.StatusNotFound, "not_found")
	var requests int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM approvals.requests WHERE kind = 'payments.suspense_credit'
		AND payload->>'order_id' IN ($1, $2)`, outgoing.Id.String(), suspended).Scan(&requests); err != nil || requests != 0 {
		t.Fatalf("credit requests = %d (%v), want none", requests, err)
	}
}

// TestASuspenseOverTheAccountsLimitsIsNotCredited (FR-24, FR-32): a credit that would take the account over a limit
// of its product leaves the request failed and the money in suspense.
func TestASuspenseOverTheAccountsLimitsIsNotCredited(t *testing.T) {
	w, a := payingWorldWith(t, func(w depositWorld) string { return w.limitedSavings(t, `{"max_balance": {"units": 110000000}}`) })
	suspense := withSuspense(t, w)
	credit := w.creditRequested(t, suspendedTransfer(t, w, newRailSide(t)), a)

	got := approve(t, w.operatorB(t), credit.ID)

	if got.Status != "failed" || balanceAt(t, w.ts, suspense, "").Natural.Amount != 30000000 || w.natural(t, a) != 100000000 {
		t.Fatalf("approve = %+v, want failed with the transfer still in suspense and the account as it was", got)
	}
}

// TestASuspenseIsNotCreditedToAnInactiveAccount (FR-22, FR-32): approving the credit after the account was frozen
// leaves the request failed and the money in suspense.
func TestASuspenseIsNotCreditedToAnInactiveAccount(t *testing.T) {
	w, a := payingWorld(t)
	suspense := withSuspense(t, w)
	id := suspendedTransfer(t, w, newRailSide(t))
	credit := w.creditRequested(t, id, a)
	w.setStatus(t, a, deposits.Frozen)

	got := approve(t, w.operatorB(t), credit.ID)

	if got.Status != "failed" || balanceAt(t, w.ts, suspense, "").Natural.Amount != 30000000 {
		t.Fatalf("approve = %+v with %d in suspense, want failed and the transfer still there", got, balanceAt(t, w.ts, suspense, "").Natural.Amount)
	}
	if status, _ := orderStatus(t, id); status != "suspended" {
		t.Fatalf("the order is %s, want it still suspended", status)
	}
}
