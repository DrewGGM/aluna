//go:build integration

package httpapi

import (
	"net/http"
	"testing"

	"github.com/DrewGGM/aluna/internal/party"
	"github.com/DrewGGM/aluna/internal/payments"
)

// creditsCounted is what the counters of account a recorded as credits, by period.
func creditsCounted(t *testing.T, a string) map[string]int64 {
	t.Helper()
	rows, err := testPool.Query(t.Context(), `SELECT period, credits FROM deposits.counters WHERE account_id = $1`, a)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]int64{}
	for rows.Next() {
		var period string
		var credits int64
		if err := rows.Scan(&period, &credits); err != nil {
			t.Fatal(err)
		}
		got[period] = credits
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

// TestATransferReceivedKeepsWithinTheAccountsLimits (FR-24, FR-32): a transfer received for an active account counts
// as a credit of it, once though the rail sends it twice; one that would take the account over a limit of its
// product goes to the rail's suspense account as LIMIT_EXCEEDED and the account keeps its balance.
func TestATransferReceivedKeepsWithinTheAccountsLimits(t *testing.T) {
	w, a := payingWorldWith(t, func(w depositWorld) string { return w.limitedSavings(t, `{"max_balance": {"units": 110000000}}`) })
	suspense := withSuspense(t, w)
	rails := newRailSide(t)
	within := signedTransfer(receivedBody(w.entity, newE2E(), 5000000, a.Id.String()))
	over := newE2E()

	for _, r := range []response{
		deliverTo(t, rails.ts, "transfers", within), deliverTo(t, rails.ts, "transfers", within),
		deliverTo(t, rails.ts, "transfers", signedTransfer(receivedBody(w.entity, over, 30000000, a.Id.String()))),
	} {
		if r.status != http.StatusNoContent {
			t.Fatalf("a transfer = %d %s, want 204", r.status, r.body)
		}
	}

	if balance, held := w.natural(t, a), balanceAt(t, w.ts, suspense, "").Natural.Amount; balance != 105000000 || held != 30000000 {
		t.Fatalf("balance = %d and suspense %d, want 1,050,000 COP and the transfer over the limit", balance, held)
	}
	var status, reason string
	if err := testPool.QueryRow(t.Context(), `SELECT status, reason FROM payments.orders WHERE end_to_end_id = $1`, over).Scan(&status, &reason); err != nil ||
		status != "suspended" || reason != payments.ReasonCreditOverLimit {
		t.Fatalf("the transfer over the limit is %s for %q (%v), want suspended for LIMIT_EXCEEDED", status, reason, err)
	}
	expectCreditsCounted(t, a.Id.String(), 100000000+5000000)
}

// expectCreditsCounted checks that the day's and the month's counters of account a recorded want of credits.
func expectCreditsCounted(t *testing.T, a string, want int64) {
	t.Helper()
	counted := creditsCounted(t, a)
	if len(counted) != 2 {
		t.Fatalf("counters = %v, want the day's and the month's", counted)
	}
	for period, credits := range counted {
		if credits != want {
			t.Errorf("credits of %s = %d, want %d", period, credits, want)
		}
	}
}

// TestATransferWhoseLimitsAreUnknownGoesToSuspense (FR-24, FR-32): a transfer for an account whose limits are in
// minimum wages, with no minimum wage loaded, is not credited nor refused: it goes to the rail's suspense account as
// LIMITS_UNAVAILABLE, on the books until someone credits it.
func TestATransferWhoseLimitsAreUnknownGoesToSuspense(t *testing.T) {
	w := newDepositWorld(t)
	if status := call(t, w.ts, http.MethodPost, "/v1/ledger-accounts",
		`{"code":"L.CLEARING.SIMBREB","kind":"liability","currency":"COP","balance_mode":"async"}`, nil); status != http.StatusCreated {
		t.Fatalf("create the clearing account = %d", status)
	}
	suspense := withSuspense(t, w)
	a := w.opened(t, w.party(t, party.KYCApproved), w.limitedSavings(t, `{"max_balance": {"smmlv": 10}}`))
	e2e := newE2E()

	if r := deliverTo(t, newRailSide(t).ts, "transfers", signedTransfer(receivedBody(w.entity, e2e, 5000000, a.Id.String()))); r.status != http.StatusNoContent {
		t.Fatalf("the transfer = %d %s, want 204", r.status, r.body)
	}

	var status, reason string
	if err := testPool.QueryRow(t.Context(), `SELECT status, reason FROM payments.orders WHERE end_to_end_id = $1`, e2e).Scan(&status, &reason); err != nil ||
		status != "suspended" || reason != payments.ReasonLimitsUnavailable {
		t.Fatalf("the transfer is %s for %q (%v), want suspended for LIMITS_UNAVAILABLE", status, reason, err)
	}
	if balance, held := w.natural(t, a), balanceAt(t, w.ts, suspense, "").Natural.Amount; balance != 0 || held != 5000000 {
		t.Fatalf("balance = %d and suspense %d, want nothing credited and the transfer in suspense", balance, held)
	}
}
