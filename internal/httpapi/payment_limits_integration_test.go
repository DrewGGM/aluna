//go:build integration

package httpapi

import (
	"net/http"
	"testing"

	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/ids"
)

// limitedPayingWorld is a paying world whose account's product limits the debits of a day to 500,000 COP.
func limitedPayingWorld(t *testing.T) (depositWorld, gen.DepositAccount) {
	t.Helper()
	return payingWorldWith(t, func(w depositWorld) string { return w.limitedSavings(t, `{"daily_debits": {"units": 50000000}}`) })
}

// TestAPaymentKeepsWithinTheAccountsLimits (FR-24, FR-31): a payment is a debit of its account: one that would
// take the day's debits over the product's limit is 422 limit_exceeded and holds nothing.
func TestAPaymentKeepsWithinTheAccountsLimits(t *testing.T) {
	w, a := limitedPayingWorld(t)
	paid(t, postTo(t, w.ts, "/v1/payment-orders", ids.New().String(), paymentBody(a, 30000000, "simbreb", "@beneficiary")))

	r := postTo(t, w.ts, "/v1/payment-orders", ids.New().String(), paymentBody(a, 30000000, "simbreb", "@beneficiary"))

	expectProblem(t, "a payment over the daily debits", r, http.StatusUnprocessableEntity, "limit_exceeded")
	if b := balanceAt(t, w.ts, a.LedgerAccountId.String(), ""); b.PendingDebits.Amount != 30120000 {
		t.Fatalf("pending debits = %d, want only the first payment's 301,200 COP", b.PendingDebits.Amount)
	}
}

// TestARejectedPaymentGivesBackItsLimit (FR-24, FR-31): a payment the rail rejects took no money, so its debit no
// longer counts against the day's limit: another payment of the same day fits again.
func TestARejectedPaymentGivesBackItsLimit(t *testing.T) {
	w, a := limitedPayingWorld(t)
	rails := newRailSide(t)
	rejected := paid(t, postTo(t, w.ts, "/v1/payment-orders", ids.New().String(), paymentBody(a, 30001300, "simbreb", "@beneficiary")))
	sentAndNotified(t, rails, rejected)
	if now := readOrder(t, w.ts, rejected.Id.String()); now.Status != gen.PaymentOrderStatusRejected {
		t.Fatalf("the first payment = %+v, want it rejected by the rail", now)
	}

	again := paid(t, postTo(t, w.ts, "/v1/payment-orders", ids.New().String(), paymentBody(a, 30000000, "simbreb", "@beneficiary")))

	if again.Status != gen.PaymentOrderStatusHeld {
		t.Fatalf("the second payment = %+v, want it held: the rejected one gave back its limit", again)
	}
}

// TestAClearedPaymentTheRailRejectsGivesBackItsLimit (FR-24, FR-42): a payment that waited for compliance counts its
// debit when it is cleared, and gives it back if the rail then rejects it.
func TestAClearedPaymentTheRailRejectsGivesBackItsLimit(t *testing.T) {
	w, a := limitedPayingWorld(t)
	rails := newRailSide(t)
	pending := paid(t, postTo(t, w.ts, "/v1/payment-orders", ids.New().String(), paymentBody(a, 30001300, "simbreb", "match-sanctioned")))
	if cleared := w.resolvePayment(t, pending, "cleared"); cleared.Status != gen.PaymentOrderStatusHeld {
		t.Fatalf("the cleared payment = %+v, want it held", cleared)
	}
	sentAndNotified(t, rails, pending)
	if now := readOrder(t, w.ts, pending.Id.String()); now.Status != gen.PaymentOrderStatusRejected {
		t.Fatalf("the cleared payment = %+v, want it rejected by the rail", now)
	}

	again := paid(t, postTo(t, w.ts, "/v1/payment-orders", ids.New().String(), paymentBody(a, 30000000, "simbreb", "@beneficiary")))

	if again.Status != gen.PaymentOrderStatusHeld {
		t.Fatalf("the second payment = %+v, want it held: the cleared and rejected one gave back its limit", again)
	}
}

// TestAClearedPaymentOverTheLimitsIsRejected (FR-24, FR-42): a payment that waited for compliance is a debit when
// it is cleared: if it would take the account's debits of the day over the limit, it is rejected as AM02.
func TestAClearedPaymentOverTheLimitsIsRejected(t *testing.T) {
	w, a := limitedPayingWorld(t)
	o := w.pendingPayment(t, a)
	expectCreated(t, "withdrawal", w.move(t, a.Id.String(), "withdrawals", amountBody(30000000)))

	rejected := w.resolvePayment(t, o, "cleared")

	if rejected.Status != gen.PaymentOrderStatusRejected || rejected.Reason == nil || *rejected.Reason != "AM02" || rejected.Held != nil {
		t.Fatalf("the order = %+v, want rejected as AM02 without a hold", rejected)
	}
}
