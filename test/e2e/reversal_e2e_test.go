//go:build e2e

package e2e

import (
	"net/http"
	"testing"
)

// TestReversalUnderMakerChecker (FR-3, FR-39): with the real binary, operator A requests the reversal of
// a transaction and cannot approve it; B approves it and the transaction is reversed: the balances go back
// to zero.
func TestReversalUnderMakerChecker(t *testing.T) {
	a := newAPI(t)
	cash, deposit := createAccount(t, a, "asset"), createAccount(t, a, "liability")
	posting := postTransaction(t, a, cash, deposit, 10_000_000)

	makerChecker(t, a, a.as("operator-b", "operator"), "/v1/transactions/"+posting+"/reversal",
		`{"reason":"posted in error","business_date":"2026-10-26"}`)

	for _, account := range []string{cash, deposit} {
		var balance struct{ Posted struct{ Amount int64 } }
		if status := call(t, a, http.MethodGet, "/v1/ledger-accounts/"+account+"/balance", "", &balance); status != http.StatusOK ||
			balance.Posted.Amount != 0 {
			t.Errorf("balance of %s after the reversal = %d %+v, want 0", account, status, balance)
		}
	}
}
