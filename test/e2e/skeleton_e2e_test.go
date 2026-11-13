//go:build e2e

// Package e2e tests the aluna binary end to end: real process, real PostgreSQL and HTTP.
package e2e

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/DrewGGM/aluna/internal/ids"
)

// TestSkeleton: a balanced transaction of 100,000 COP travels HTTP → ledger → PostgreSQL → HTTP with the
// compiled binary, migrated by itself and connected with the application role.
func TestSkeleton(t *testing.T) {
	api := newAPI(t)

	cash, deposit := createAccount(t, api, "asset"), createAccount(t, api, "liability")
	postTransaction(t, api, cash, deposit, 10_000_000)

	for account, posted := range map[string]int64{cash: 10_000_000, deposit: -10_000_000} {
		var b struct{ Posted, Natural struct{ Amount int64 } }
		expect(t, api, http.MethodGet, "/v1/ledger-accounts/"+account+"/balance", "", http.StatusOK, &b)
		if b.Posted.Amount != posted || b.Natural.Amount != 10_000_000 {
			t.Errorf("balance of %s = %+v, want signed %d and natural 100,000 COP", account, b, posted)
		}
	}
}

// postTransaction posts units from the debit account against the credit account and returns the
// transaction ID.
func postTransaction(t *testing.T, a runningAPI, debit, credit string, units int64) string {
	t.Helper()
	posting := fmt.Sprintf(`{"code":"deposit","value_date":"2026-10-26","business_date":"2026-10-26",
		"source":"e2e","external_ref":%q,"entries":[
		{"account_id":%q,"amount":{"amount":%d,"currency":"COP"}},
		{"account_id":%q,"amount":{"amount":%d,"currency":"COP"}}]}`, ids.New().String(), debit, units, credit, -units)
	var posted struct{ ID string }
	expect(t, a, http.MethodPost, "/v1/transactions", posting, http.StatusCreated, &posted)
	return posted.ID
}

func createAccount(t *testing.T, a runningAPI, kind string) string {
	t.Helper()
	var account struct{ ID string }
	body := fmt.Sprintf(`{"code":"e2e-%s","kind":%q,"currency":"COP","balance_mode":"sync"}`, ids.New(), kind)
	expect(t, a, http.MethodPost, "/v1/ledger-accounts", body, http.StatusCreated, &account)
	return account.ID
}
