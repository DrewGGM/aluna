//go:build integration

package httpapi

import (
	"net/http"
	"testing"

	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/outbox"
)

// pendingPayment pays 300,000 COP from a to a beneficiary screening may match: the order must be pending_screening.
func (w depositWorld) pendingPayment(t *testing.T, a gen.DepositAccount) gen.PaymentOrder {
	t.Helper()
	o := paid(t, postTo(t, w.ts, "/v1/payment-orders", ids.New().String(), paymentBody(a, 30000000, "simbreb", "match-sanctioned")))
	if o.Status != gen.PaymentOrderStatusPendingScreening {
		t.Fatalf("pay a possible match = %+v, want pending_screening", o)
	}
	return o
}

// resolvePayment has compliance officer A request the resolution of o's case and officer B approve it, and
// returns the order as it stands afterwards.
func (w depositWorld) resolvePayment(t *testing.T, o gen.PaymentOrder, resolution string) gen.PaymentOrder {
	t.Helper()
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	a, b := apiServerAsRole(t, w.entity, events, "officer-a", "compliance"), apiServerAsRole(t, w.entity, events, "officer-b", "compliance")
	var caseID ids.ID
	if err := testPool.QueryRow(t.Context(), `SELECT id FROM compliance.cases WHERE subject_id = $1`, o.Id).Scan(&caseID); err != nil {
		t.Fatal(err)
	}
	requested := postTo(t, a, "/v1/screening-cases/"+caseID.String()+"/resolution", ids.New().String(), `{"resolution": "`+resolution+`"}`)
	if requested.status != http.StatusAccepted {
		t.Fatalf("request the resolution = %d %s, want 202", requested.status, requested.body)
	}
	if got := approve(t, b, decodeApproval(t, requested).ID); got.Status != "approved" {
		t.Fatalf("approve the resolution = %+v, want approved", got)
	}
	return readOrder(t, w.ts, o.Id.String())
}

// quotesOf is how many quotes account a has, used or not.
func quotesOf(t *testing.T, a gen.DepositAccount) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM pricing.quotes WHERE account_id = $1`, a.Id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// sendJobs is how many payments.send jobs order o has.
func sendJobs(t *testing.T, o gen.PaymentOrder) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM infra.river_job WHERE kind = 'payments.send' AND args->>'order_id' = $1`,
		o.Id.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestAClearedPaymentIsHeldAndSent (FR-31, FR-42): once compliance clears its case, a payment that waited is held
// with its charges, as one screening cleared at once, and queued to be sent.
func TestAClearedPaymentIsHeldAndSent(t *testing.T) {
	w, a := payingWorld(t)
	o := w.pendingPayment(t, a)

	cleared := w.resolvePayment(t, o, "cleared")

	if cleared.Status != gen.PaymentOrderStatusHeld || cleared.Held == nil || cleared.Held.Amount != 30120000 {
		t.Fatalf("the cleared order = %+v, want it held for 301,200 COP", cleared)
	}
	if jobs := sendJobs(t, cleared); jobs != 1 {
		t.Fatalf("send jobs = %d, want 1", jobs)
	}
	if balance := w.natural(t, a); balance != 100000000 {
		t.Fatalf("balance = %d, want it untouched until the payment settles", balance)
	}
}

// TestAPaymentThatWaitedIsRejectedWhenItCannotGo (FR-31, FR-42): a confirmed match rejects the order (RR04); a
// cleared one is rejected if its account no longer has the funds (AM04) or is no longer active (AC06). None holds
// anything, is sent, or leaves the charges it priced (a quote) behind.
func TestAPaymentThatWaitedIsRejectedWhenItCannotGo(t *testing.T) {
	for name, c := range map[string]struct {
		resolution, reason string
		meanwhile          func(w depositWorld, a gen.DepositAccount)
	}{
		"a confirmed match": {"confirmed", "RR04", func(depositWorld, gen.DepositAccount) {}},
		"without funds": {"cleared", "AM04", func(w depositWorld, a gen.DepositAccount) {
			expectCreated(t, "withdrawal", w.move(t, a.Id.String(), "withdrawals", amountBody(99000000)))
		}},
		"a frozen account": {"cleared", "AC06", func(w depositWorld, a gen.DepositAccount) { w.setStatus(t, a, deposits.Frozen) }},
	} {
		t.Run(name, func(t *testing.T) {
			w, a := payingWorld(t)
			o := w.pendingPayment(t, a)
			c.meanwhile(w, a)
			before, quotes := w.natural(t, a), quotesOf(t, a)

			rejected := w.resolvePayment(t, o, c.resolution)

			if rejected.Status != gen.PaymentOrderStatusRejected || rejected.Reason == nil || *rejected.Reason != c.reason || rejected.Held != nil {
				t.Fatalf("the order = %+v, want rejected for %s without a hold", rejected, c.reason)
			}
			if jobs, after, quotesAfter := sendJobs(t, rejected), w.natural(t, a), quotesOf(t, a); jobs != 0 || after != before || quotesAfter != quotes {
				t.Fatalf("send jobs = %d, balance %d and quotes %d, want none, %d and %d", jobs, after, quotesAfter, before, quotes)
			}
		})
	}
}
