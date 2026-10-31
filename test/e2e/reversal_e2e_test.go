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
	b := a.as("operator-b")
	cash, deposit := createAccount(t, a, "asset"), createAccount(t, a, "liability")
	posting := postTransaction(t, a, cash, deposit, 10_000_000)

	var approval struct{ ID, Status string }
	status := call(t, a, http.MethodPost, "/v1/transactions/"+posting+"/reversal",
		`{"reason":"posted in error","business_date":"2026-10-26"}`, &approval)
	if status != http.StatusAccepted || approval.Status != "pending" {
		t.Fatalf("A requests the reversal = %d %+v, want 202 pending", status, approval)
	}
	var self struct{ Code string }
	if status := call(t, a, http.MethodPost, "/v1/approvals/"+approval.ID+"/approval", "", &self); status != http.StatusForbidden ||
		self.Code != "self_approval_forbidden" {
		t.Fatalf("A approves its own reversal = %d %+v, want 403 self_approval_forbidden", status, self)
	}
	var approved struct{ Status string }
	if status := call(t, b, http.MethodPost, "/v1/approvals/"+approval.ID+"/approval", "", &approved); status != http.StatusOK ||
		approved.Status != "approved" {
		t.Fatalf("B approves = %d %+v, want 200 approved", status, approved)
	}

	for _, account := range []string{cash, deposit} {
		var balance struct{ Posted struct{ Amount int64 } }
		if status := call(t, a, http.MethodGet, "/v1/ledger-accounts/"+account+"/balance", "", &balance); status != http.StatusOK ||
			balance.Posted.Amount != 0 {
			t.Errorf("balance of %s after the reversal = %d %+v, want 0", account, status, balance)
		}
	}
}
