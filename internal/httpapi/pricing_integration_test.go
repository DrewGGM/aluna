//go:build integration

package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/party"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/pricing"
	"github.com/DrewGGM/aluna/internal/products"
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

// TestAQuotedTransferOverHTTP (FR-28, US-7): a transfer with the quote of its origin is charged exactly what
// it showed, its fee and its GMF; the same quote again is 409 quote_unavailable and moves nothing.
func TestAQuotedTransferOverHTTP(t *testing.T) {
	w := newDepositWorld(t)
	from, to := w.tiered(t), w.tiered(t)
	expectCreated(t, "deposit", w.move(t, from.Id.String(), "deposits", amountBody(100000000)))
	q := quoted(t, w.ts, quoteBody(from.Id.String(), "transfer", 10000000))
	body := strings.Replace(transferBody(from, to, 10000000), `{`, `{"quote_id":"`+q.Id.String()+`",`, 1)

	transferred := postTo(t, w.ts, "/v1/transfers", ids.New().String(), body)
	again := postTo(t, w.ts, "/v1/transfers", ids.New().String(), body)

	if q.TotalFees.Amount != 300000 || q.Gmf != (gen.Money{Amount: 40000, Currency: "COP"}) {
		t.Fatalf("quote = %+v, want the transfer fee of 3,000 COP and 400 COP of GMF", q)
	}
	expectCreated(t, "transfer", transferred)
	if again.status != http.StatusConflict || problemCode(t, again) != "quote_unavailable" {
		t.Fatalf("the same quote again = %d %s, want 409 quote_unavailable", again.status, again.body)
	}
	if w.natural(t, from) != 89660000 || w.natural(t, to) != 10000000 {
		t.Fatalf("balances %d and %d, want 896,600 and 100,000 COP", w.natural(t, from), w.natural(t, to))
	}
}

// exemption asks to exempt account id from GMF, without Idempotency-Key.
func exemption(t *testing.T, ts *httptest.Server, id string) response {
	t.Helper()
	r, err := send(t.Context(), ts, "/v1/deposit-accounts/"+id+"/gmf-exemption", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestAnExemptionOverHTTP (FR-29): an account of a product that allows it becomes exempt, and its quotes show
// no GMF while its party's exemption covers them. On a single account, the same account or another one of
// the party is 409 gmf_exemption_taken; a product that does not allow it is 422 gmf_not_eligible; an unknown
// account, 404.
func TestAnExemptionOverHTTP(t *testing.T) {
	w := newDepositWorld(t)
	w.uvt(t)
	customer, savings := w.party(t, party.KYCApproved), w.savings(t, true)
	a, b := w.opened(t, customer, savings), w.opened(t, customer, savings)

	marked := exemption(t, w.ts, a.Id.String())
	var e gen.GMFExemption
	if err := json.Unmarshal(marked.body, &e); err != nil || marked.status != http.StatusCreated || e.AccountId != a.Id || e.PartyId.String() != customer {
		t.Fatalf("exemption = %d %s, want 201 for the account of the party", marked.status, marked.body)
	}
	if q := quoted(t, w.ts, quoteBody(a.Id.String(), "withdrawal", 100000000)); q.Gmf.Amount != 0 {
		t.Fatalf("quote of the exempt account = %+v, want no GMF", q)
	}
	for name, c := range map[string]struct {
		id     string
		status int
		code   string
	}{
		"the same account":        {a.Id.String(), http.StatusConflict, "gmf_exemption_taken"},
		"another account":         {b.Id.String(), http.StatusConflict, "gmf_exemption_taken"},
		"a product that does not": {w.tiered(t).Id.String(), http.StatusUnprocessableEntity, "gmf_not_eligible"},
		"an unknown account":      {ids.New().String(), http.StatusNotFound, "not_found"},
	} {
		if r := exemption(t, w.ts, c.id); r.status != c.status || problemCode(t, r) != c.code {
			t.Errorf("%s = %d %s, want %d %s", name, r.status, r.body, c.status, c.code)
		}
	}
}

// uvt loads the UVT of 2026, 52,374 pesos, unless another test of the package's database already did.
func (w depositWorld) uvt(t *testing.T) {
	t.Helper()
	uvt, err := money.ParsePrecise("52374")
	if err != nil {
		t.Fatal(err)
	}
	err = postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		return w.products.LoadMarketParameter(t.Context(), tx, products.MarketParameter{
			Kind: products.UVT, ValidFrom: calendar.Date{Year: 2026, Month: time.January, Day: 1},
			ValidTo: calendar.Date{Year: 2027, Month: time.January, Day: 1}, Value: uvt,
		})
	})
	if err != nil && !errors.Is(err, products.ErrMarketParameterOverlap) {
		t.Fatal(err)
	}
}
