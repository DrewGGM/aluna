//go:build integration

package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/party"
	"github.com/DrewGGM/aluna/internal/pricing"
)

// quoteBody is the body of a quote of units COP of operation op on account.
func quoteBody(account, op string, units int64) string {
	return `{"account_id":"` + account + `","operation":"` + op + `","amount":{"amount":` + strconv.FormatInt(units, 10) + `,"currency":"COP"}}`
}

// quoteOf asks for a quote with body, without Idempotency-Key: a quote moves no money.
func quoteOf(t *testing.T, ts *httptest.Server, body string) response {
	t.Helper()
	r, err := send(t.Context(), ts, "/v1/quotes", nil, body)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// quoted is the quote that body asks for, which must be made.
func quoted(t *testing.T, ts *httptest.Server, body string) gen.Quote {
	t.Helper()
	r := quoteOf(t, ts, body)
	var q gen.Quote
	if r.status != http.StatusCreated {
		t.Fatalf("quote = %d %s", r.status, r.body)
	}
	if err := json.Unmarshal(r.body, &q); err != nil {
		t.Fatal(err)
	}
	return q
}

// tiered opens an account of the tiered savings product of products' testdata: its first three withdrawals of
// the month are free and a transfer pays 3,000 COP.
func (w depositWorld) tiered(t *testing.T) gen.DepositAccount {
	t.Helper()
	definition, err := os.ReadFile(filepath.Join("..", "products", "internal", "domain", "testdata", "valid-tiered-savings.json"))
	if err != nil {
		t.Fatal(err)
	}
	return w.opened(t, w.party(t, party.KYCApproved), w.product(t, definition, true))
}

// TestAQuoteOverHTTP (FR-28, US-7): a quote of an account shows the fees of the operation, with their
// exemptions decided on the account: its first withdrawals of the month are free, a transfer pays 3,000 COP.
func TestAQuoteOverHTTP(t *testing.T) {
	w := newDepositWorld(t)
	a := w.tiered(t)
	id := a.Id.String()

	withdrawal := quoted(t, w.ts, quoteBody(id, "withdrawal", 1000000))
	transfer := quoted(t, w.ts, quoteBody(id, "transfer", 1000000))

	if len(withdrawal.Fees) != 0 || withdrawal.TotalFees.Amount != 0 || withdrawal.AccountId != a.Id {
		t.Fatalf("withdrawal quote = %+v, want it free for the account", withdrawal)
	}
	fee := gen.Money{Amount: 300000, Currency: "COP"}
	if len(transfer.Fees) != 1 || transfer.Fees[0] != (gen.QuotedFee{Code: "transfer_fee", Amount: fee}) || transfer.TotalFees != fee {
		t.Fatalf("transfer quote = %+v, want the transfer fee of 3,000 COP", transfer)
	}
	if _, offset := transfer.ExpiresAt.Zone(); offset != 0 {
		t.Fatalf("expires_at = %s, want it in UTC", transfer.ExpiresAt)
	}
}

// TestARejectedQuoteNamesItsAccount (FR-40): a quote whose body is rejected (an unknown currency) is audited
// with the rejection and the account it named.
func TestARejectedQuoteNamesItsAccount(t *testing.T) {
	w := newDepositWorld(t)
	id := w.tiered(t).Id.String()

	resp, correlation := auditedRequest(t, w.ts, http.MethodPost, "/v1/quotes", "", strings.Replace(quoteBody(id, "transfer", 100), "COP", "XXX", 1))

	got := onlyRecord(t, correlation)
	if resp.status/100 != 4 || got.Result != codeOf(t, resp) || !slices.Equal(got.Targets, []string{id}) {
		t.Fatalf("response %d %s; record %+v, want the rejection with the account", resp.status, resp.body, got.Record)
	}
}

// TestWhatCannotBeQuotedIsAProblem: an unknown account, an operation that is not priced and an amount that is
// not positive come back as their problems.
func TestWhatCannotBeQuotedIsAProblem(t *testing.T) {
	w := newDepositWorld(t)
	id := w.tiered(t).Id.String()
	for name, c := range map[string]struct {
		body   string
		status int
		code   string
	}{
		"an unknown account": {quoteBody(ids.New().String(), "transfer", 100), http.StatusNotFound, "not_found"},
		"a deposit":          {quoteBody(id, "deposit", 100), http.StatusUnprocessableEntity, "invalid_quote"},
		"nothing":            {quoteBody(id, "transfer", 0), http.StatusUnprocessableEntity, "invalid_quote"},
	} {
		r := quoteOf(t, w.ts, c.body)
		if r.status != c.status || problemCode(t, r) != c.code {
			t.Errorf("%s = %d %s, want %d %s", name, r.status, r.body, c.status, c.code)
		}
	}
}

// TestAnExemptionReadsTheAccount (FR-28): the facts a product's rule reads come from the account: here the
// transfer fee is waived for a natural person of low risk with at least 1,000 COP, so the same account pays it
// before its deposit and not after.
func TestAnExemptionReadsTheAccount(t *testing.T) {
	w := newDepositWorld(t)
	var definition map[string]json.RawMessage
	if err := json.Unmarshal(savingsDefinition(t), &definition); err != nil {
		t.Fatal(err)
	}
	definition["fees"] = json.RawMessage(`[{"code": "transfer_fee", "trigger": "transfer", "amount_units": 300000, "exempt_rule": "good_customer"}]`)
	definition["rules"] = json.RawMessage(`{"good_customer": "party_kind == 'natural' && risk_level == 'low' && balance_units >= 100000"}`)
	document, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	a := w.opened(t, w.party(t, party.KYCApproved), w.product(t, document, true))
	quote := func() int { return len(quoted(t, w.ts, quoteBody(a.Id.String(), "transfer", 1000)).Fees) }

	before := quote()
	expectCreated(t, "deposit", w.move(t, a.Id.String(), "deposits", amountBody(100000)))
	after := quote()

	if before != 1 || after != 0 {
		t.Fatalf("transfer fees before the deposit %d, after %d; want 1, then waived", before, after)
	}
}

// TestAnUnavailableQuoteIsAConflict: the operation that uses a quote already used, expired or of another
// operation answers 409 quote_unavailable.
func TestAnUnavailableQuoteIsAConflict(t *testing.T) {
	p, ok := problemFor(fmt.Errorf("charge: %w", pricing.ErrQuoteUnavailable))
	if !ok || p.Status != http.StatusConflict || p.Code != "quote_unavailable" {
		t.Fatalf("problem = %+v (%v), want 409 quote_unavailable", p, ok)
	}
}
