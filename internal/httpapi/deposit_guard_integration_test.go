//go:build integration

package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/party"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// manualPosting is the body of a manual transaction of units from debit to credit on the deposit world's business
// date.
func manualPosting(t *testing.T, debit, credit string, units int64) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"code": "adjustment", "value_date": "2026-03-02", "business_date": "2026-03-02", "source": "test",
		"external_ref": ids.New().String(),
		"entries": []map[string]any{
			{"account_id": debit, "amount": map[string]any{"amount": units, "currency": "COP"}},
			{"account_id": credit, "amount": map[string]any{"amount": -units, "currency": "COP"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// setStatus moves account a to status directly, as the API or the dormancy job would.
func (w depositWorld) setStatus(t *testing.T, a gen.DepositAccount, status deposits.Status) {
	t.Helper()
	w.do(t, func(tx postgres.Tx) error {
		return w.deposits.ChangeStatus(t.Context(), tx, w.entity, ids.ID(a.Id), status, "operator-a")
	})
}

// TestNoPostingOrHoldTouchesAnInactiveAccount (FR-22): a manual transaction, or a hold on or against the ledger
// account of a frozen, dormant or closed deposit account, is 409 account_not_active and leaves its balance as it
// was; on an active one it posts.
func TestNoPostingOrHoldTouchesAnInactiveAccount(t *testing.T) {
	w := newDepositWorld(t)
	approved, savings := w.party(t, party.KYCApproved), w.savings(t, true)
	active, frozen, dormant, closed := w.opened(t, approved, savings), w.opened(t, approved, savings),
		w.opened(t, approved, savings), w.opened(t, approved, savings)
	for _, a := range []gen.DepositAccount{frozen, dormant} {
		expectCreated(t, "deposit", w.move(t, a.Id.String(), "deposits", amountBody(1000)))
	}
	w.setStatus(t, frozen, deposits.Frozen)
	w.setStatus(t, dormant, deposits.Dormant)
	w.setStatus(t, closed, deposits.Closed)
	other := createAccount(t, w.ts, "asset")

	for status, a := range map[string]gen.DepositAccount{"frozen": frozen, "dormant": dormant, "closed": closed} {
		ledgerAccount := a.LedgerAccountId.String()
		posted := postTo(t, w.ts, "/v1/transactions", ids.New().String(), manualPosting(t, ledgerAccount, other, 100))
		expectProblem(t, "a manual debit of the "+status+" account", posted, http.StatusConflict, "account_not_active")
		credited := postTo(t, w.ts, "/v1/transactions", ids.New().String(), manualPosting(t, other, ledgerAccount, 100))
		expectProblem(t, "a manual credit to the "+status+" account", credited, http.StatusConflict, "account_not_active")
		held := postTo(t, w.ts, "/v1/holds", ids.New().String(), holdBody(ledgerAccount, other, 100))
		expectProblem(t, "a hold on the "+status+" account", held, http.StatusConflict, "account_not_active")
		against := postTo(t, w.ts, "/v1/holds", ids.New().String(), holdBody(active.LedgerAccountId.String(), ledgerAccount, 100))
		expectProblem(t, "a hold against the "+status+" account", against, http.StatusConflict, "account_not_active")
	}
	for a, want := range map[*gen.DepositAccount]int64{&frozen: 1000, &dormant: 1000, &closed: 0} {
		if got := w.natural(t, *a); got != want {
			t.Errorf("balance of the %s account = %d, want %d", a.Id, got, want)
		}
	}
	expectCreated(t, "a manual credit to an active account",
		postTo(t, w.ts, "/v1/transactions", ids.New().String(), manualPosting(t, other, active.LedgerAccountId.String(), 100)))
}

// TestACaptureOnAFrozenAccountIsRefused (FR-22): a hold placed while the account was active is not captured once it
// is frozen: 409 account_not_active, and the money stays.
func TestACaptureOnAFrozenAccountIsRefused(t *testing.T) {
	w := newDepositWorld(t)
	a := w.opened(t, w.party(t, party.KYCApproved), w.savings(t, true))
	expectCreated(t, "deposit", w.move(t, a.Id.String(), "deposits", amountBody(1000)))
	hold := placeHoldOver(t, w.ts, a.LedgerAccountId.String(), createAccount(t, w.ts, "liability"), 500)
	w.setStatus(t, a, deposits.Frozen)

	captured := postTo(t, w.ts, "/v1/holds/"+hold.Id.String()+"/capture", ids.New().String(), `{"amount":{"amount":500,"currency":"COP"},"business_date":"2026-03-02"}`)

	expectProblem(t, "capture on a frozen account", captured, http.StatusConflict, "account_not_active")
	if natural := w.natural(t, a); natural != 1000 {
		t.Fatalf("balance = %d, want 1000", natural)
	}
}

// TestAReversalOnAFrozenAccountFails (FR-3, FR-22): the reversal of a transaction on an account frozen after it was
// requested leaves the request failed and the money where it was.
func TestAReversalOnAFrozenAccountFails(t *testing.T) {
	w := newDepositWorld(t)
	a := w.opened(t, w.party(t, party.KYCApproved), w.savings(t, true))
	var posted gen.Transaction
	if status := call(t, w.ts, http.MethodPost, "/v1/transactions", manualPosting(t, createAccount(t, w.ts, "asset"), a.LedgerAccountId.String(), 300), &posted); status != http.StatusCreated {
		t.Fatalf("post = %d", status)
	}
	requested := postTo(t, w.ts, "/v1/transactions/"+posted.Id.String()+"/reversal", ids.New().String(),
		`{"reason": "posted twice", "business_date": "2026-03-02"}`)
	if requested.status != http.StatusAccepted {
		t.Fatalf("request reversal = %d %s, want 202", requested.status, requested.body)
	}
	w.setStatus(t, a, deposits.Frozen)

	got := approve(t, w.operatorB(t), decodeApproval(t, requested).ID)

	if got.Status != "failed" || w.natural(t, a) != 300 {
		t.Fatalf("approve = %+v with a balance of %d, want failed and 300", got, w.natural(t, a))
	}
}
