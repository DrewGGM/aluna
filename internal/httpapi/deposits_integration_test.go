//go:build integration

package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/compliance"
	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/party"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
	"github.com/DrewGGM/aluna/internal/pricing"
	"github.com/DrewGGM/aluna/internal/products"
)

// depositWorld is an entity on 2026-03-02 with the cash vault its products name, driven over HTTP; the
// parties and products it needs are created through their facades, whose own flows are tested apart.
type depositWorld struct {
	ts       *httptest.Server
	entity   ids.ID
	parties  *party.Service
	products *products.Service
	deposits *deposits.Service
}

func newDepositWorld(t *testing.T) depositWorld {
	t.Helper()
	entity := ids.New()
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO calendar.legal_entities (id, name, business_date, functional_currency)
			VALUES ($1, 'Entity', '2026-03-02', 'COP')`, entity)
		return err
	})
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	w := depositWorld{
		ts: apiServerWith(t, entity, events), entity: entity, parties: party.New(events, compliance.New(events).PartyScreener()),
		products: products.New(events),
	}
	w.deposits = deposits.New(ledger.New(calendar.New(), events), w.parties, w.products, calendar.New(),
		pricing.New(w.products, calendar.New(), pricing.SingleAccount), events)
	var vault gen.LedgerAccount
	if status := call(t, w.ts, http.MethodPost, "/v1/ledger-accounts",
		`{"code":"A.CASH.VAULT","kind":"asset","currency":"COP","balance_mode":"sync"}`, &vault); status != http.StatusCreated {
		t.Fatalf("create the vault = %d", status)
	}
	for _, account := range []string{
		`{"code":"I.FEES.SAVINGS","kind":"income","currency":"COP","balance_mode":"async"}`,
		`{"code":"I.FEES.TIERED","kind":"income","currency":"COP","balance_mode":"async"}`,
		`{"code":"L.TAX.GMF","kind":"liability","currency":"COP","balance_mode":"async"}`,
	} {
		if status := call(t, w.ts, http.MethodPost, "/v1/ledger-accounts", account, nil); status != http.StatusCreated {
			t.Fatalf("create %s = %d", account, status)
		}
	}
	return w
}

func (w depositWorld) do(t *testing.T, fn func(tx postgres.Tx) error) {
	t.Helper()
	if err := postgres.NewUnitOfWork(testPool).Do(t.Context(), fn); err != nil {
		t.Fatal(err)
	}
}

// party registers a party with KYC kyc (pending, or approved through a change).
func (w depositWorld) party(t *testing.T, kyc party.KYCStatus) string {
	t.Helper()
	var p party.Party
	w.do(t, func(tx postgres.Tx) error {
		var err error
		p, err = w.parties.Register(t.Context(), tx, party.RegisterInput{
			LegalEntityID: w.entity, Kind: party.KindNatural, ExternalRef: "onboarding-" + ids.New().String(), RiskLevel: party.RiskLow,
		})
		if err != nil || kyc == party.KYCPending {
			return err
		}
		_, err = w.parties.ChangeKYC(t.Context(), tx, w.entity, p.ID, 1, kyc)
		return err
	})
	return p.ID.String()
}

// savingsDefinition is the definition of the savings product of products' testdata.
func savingsDefinition(t *testing.T) []byte {
	t.Helper()
	definition, err := os.ReadFile(filepath.Join("..", "products", "internal", "domain", "testdata", "valid-savings.json"))
	if err != nil {
		t.Fatal(err)
	}
	return definition
}

// savings creates a savings product and, if publish, publishes it.
func (w depositWorld) savings(t *testing.T, publish bool) string {
	t.Helper()
	return w.product(t, savingsDefinition(t), publish)
}

// limitedSavings creates and publishes the savings product with limits (a JSON object) instead of its own.
func (w depositWorld) limitedSavings(t *testing.T, limits string) string {
	t.Helper()
	var definition map[string]json.RawMessage
	if err := json.Unmarshal(savingsDefinition(t), &definition); err != nil {
		t.Fatal(err)
	}
	definition["limits"] = json.RawMessage(limits)
	document, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	return w.product(t, document, true)
}

// product creates a product with definition and, if publish, publishes it.
func (w depositWorld) product(t *testing.T, definition []byte, publish bool) string {
	t.Helper()
	var p products.Product
	w.do(t, func(tx postgres.Tx) error {
		var err error
		if p, err = w.products.CreateProduct(t.Context(), tx, w.entity, "p"+strings.ReplaceAll(ids.New().String(), "-", ""), "Savings account"); err != nil {
			return err
		}
		v, err := w.products.CreateVersion(t.Context(), tx, w.entity, p.ID, "product-designer", definition)
		if err != nil || !publish {
			return err
		}
		if _, err = w.products.RequestPublication(t.Context(), tx, w.entity, v.ID, "product-designer"); err != nil {
			return err
		}
		_, err = w.products.Publish(t.Context(), tx, w.entity, v.ID)
		return err
	})
	return p.ID.String()
}

func (w depositWorld) open(t *testing.T, partyID, productID string) response {
	t.Helper()
	return postTo(t, w.ts, "/v1/deposit-accounts", ids.New().String(), `{"party_id":"`+partyID+`","product_id":"`+productID+`"}`)
}

// opened opens an account, which must open (201).
func (w depositWorld) opened(t *testing.T, partyID, productID string) gen.DepositAccount {
	t.Helper()
	r := w.open(t, partyID, productID)
	var a gen.DepositAccount
	if err := json.Unmarshal(r.body, &a); err != nil || r.status != http.StatusCreated {
		t.Fatalf("open = %d %s (%v), want 201", r.status, r.body, err)
	}
	return a
}

func amountBody(units int64) string {
	return `{"amount":{"amount":` + strconv.FormatInt(units, 10) + `,"currency":"COP"}}`
}

func transferBody(from, to gen.DepositAccount, units int64) string {
	return `{"from_account_id":"` + from.Id.String() + `","to_account_id":"` + to.Id.String() + `","amount":{"amount":` + strconv.FormatInt(units, 10) + `,"currency":"COP"}}`
}

func (w depositWorld) natural(t *testing.T, a gen.DepositAccount) int64 {
	t.Helper()
	return balanceAt(t, w.ts, a.LedgerAccountId.String(), "").Natural.Amount
}

// move does POST /v1/deposit-accounts/{id}/action with body and a new Idempotency-Key.
func (w depositWorld) move(t *testing.T, id, action, body string) response {
	t.Helper()
	return postTo(t, w.ts, "/v1/deposit-accounts/"+id+"/"+action, ids.New().String(), body)
}

func expectCreated(t *testing.T, what string, r response) {
	t.Helper()
	if r.status != http.StatusCreated {
		t.Fatalf("%s = %d %s, want 201", what, r.status, r.body)
	}
}

// TestAnAccountOpensOverHTTP (FR-22): an opened account is active, in the product's currency, on the
// business date, for its party, and reads the same.
func TestAnAccountOpensOverHTTP(t *testing.T) {
	w := newDepositWorld(t)
	customer := w.party(t, party.KYCApproved)

	opened := w.opened(t, customer, w.savings(t, true))

	var read gen.DepositAccount
	if status := call(t, w.ts, http.MethodGet, "/v1/deposit-accounts/"+opened.Id.String(), "", &read); status != http.StatusOK || read != opened {
		t.Fatalf("read = %d %+v, want 200 %+v", status, read, opened)
	}
	if opened.Status != gen.DepositAccountStatusActive || opened.Currency != "COP" || opened.OpenedOn.String() != "2026-03-02" ||
		opened.LastCustomerActivityOn != opened.OpenedOn || opened.PartyId.String() != customer {
		t.Fatalf("opened = %+v, want it active in COP on 2026-03-02 for %s", opened, customer)
	}
}

// TestMoneyMovesOverHTTP (US-5, FR-23): deposit 100,000 into an account, retry the deposit with the same
// key (the same response, posted once), transfer 30,000 to another account, which costs the first its GMF of
// 120 (FR-29), and withdraw 60,000 from the first, with its GMF of 240: balances 9,640 and 30,000.
func TestMoneyMovesOverHTTP(t *testing.T) {
	w := newDepositWorld(t)
	customer, savings := w.party(t, party.KYCApproved), w.savings(t, true)
	first, second := w.opened(t, customer, savings), w.opened(t, customer, savings)
	path, key := "/v1/deposit-accounts/"+first.Id.String()+"/deposits", ids.New().String()

	deposited, retried := postTo(t, w.ts, path, key, amountBody(10000000)), postTo(t, w.ts, path, key, amountBody(10000000))
	expectCreated(t, "deposit", deposited)
	if retried.status != http.StatusCreated || !bytes.Equal(retried.body, deposited.body) {
		t.Fatalf("retry = %d %s, want 201 with the deposit's body %s", retried.status, retried.body, deposited.body)
	}
	expectCreated(t, "transfer", postTo(t, w.ts, "/v1/transfers", ids.New().String(), transferBody(first, second, 3000000)))
	if w.natural(t, first) != 6988000 || w.natural(t, second) != 3000000 {
		t.Fatalf("after the transfer, balances %d and %d, want 6988000 and 3000000", w.natural(t, first), w.natural(t, second))
	}
	expectCreated(t, "withdrawal", w.move(t, first.Id.String(), "withdrawals", amountBody(6000000)))
	if w.natural(t, first) != 964000 {
		t.Fatalf("after the withdrawal, balance %d, want 964000", w.natural(t, first))
	}
}

// TestTheChannelTellsCashApart (FR-43): a deposit and a withdrawal post on the cash channel, the one the cash
// totals follow, and a transfer does not.
func TestTheChannelTellsCashApart(t *testing.T) {
	w := newDepositWorld(t)
	customer, savings := w.party(t, party.KYCApproved), w.savings(t, true)
	first, second := w.opened(t, customer, savings), w.opened(t, customer, savings)
	id := first.Id.String()

	for what, c := range map[string]struct {
		posted response
		want   string
	}{
		"deposit":    {w.move(t, id, "deposits", amountBody(10000000)), "cash"},
		"withdrawal": {w.move(t, id, "withdrawals", amountBody(1000000)), "cash"},
		"transfer":   {postTo(t, w.ts, "/v1/transfers", ids.New().String(), transferBody(first, second, 1000000)), "api"},
	} {
		expectCreated(t, what, c.posted)
		var tx gen.Transaction
		if err := json.Unmarshal(c.posted.body, &tx); err != nil {
			t.Fatal(err)
		}
		var channel string
		if err := testPool.QueryRow(t.Context(), `SELECT channel FROM ledger.transactions WHERE id = $1`, tx.Id).Scan(&channel); err != nil {
			t.Fatal(err)
		}
		if channel != c.want {
			t.Errorf("%s posted on channel %q, want %q", what, channel, c.want)
		}
	}
}

// TestDepositErrorsAreProblems: what the deposit rules reject comes back as its problem.
func TestDepositErrorsAreProblems(t *testing.T) {
	w := newDepositWorld(t)
	approved, pending := w.party(t, party.KYCApproved), w.party(t, party.KYCPending)
	savings, draft := w.savings(t, true), w.savings(t, false)
	a, frozen := w.opened(t, approved, savings), w.opened(t, approved, savings)
	w.do(t, func(tx postgres.Tx) error {
		return w.deposits.ChangeStatus(t.Context(), tx, w.entity, ids.ID(frozen.Id), deposits.Frozen, "operator-a")
	})
	id, frozenID := a.Id.String(), frozen.Id.String()

	for name, c := range map[string]struct {
		got    response
		status int
		code   string
	}{
		"deposit into a frozen account":   {w.move(t, frozenID, "deposits", amountBody(100)), http.StatusConflict, "account_not_active"},
		"withdraw from a frozen account":  {w.move(t, frozenID, "withdrawals", amountBody(100)), http.StatusConflict, "account_not_active"},
		"deposit nothing":                 {w.move(t, id, "deposits", amountBody(0)), http.StatusUnprocessableEntity, "invalid_movement"},
		"withdraw without funds":          {w.move(t, id, "withdrawals", amountBody(100)), http.StatusUnprocessableEntity, "insufficient_funds"},
		"deposit into an unknown account": {w.move(t, ids.New().String(), "deposits", amountBody(100)), http.StatusNotFound, "not_found"},
		"transfer to itself": {
			postTo(t, w.ts, "/v1/transfers", ids.New().String(), transferBody(a, a, 100)), http.StatusUnprocessableEntity, "invalid_movement",
		},
		"open with a KYC the product does not allow": {w.open(t, pending, savings), http.StatusUnprocessableEntity, "kyc_not_satisfied"},
		"open a product without a published version": {w.open(t, approved, draft), http.StatusConflict, "no_published_version"},
	} {
		// not expectProblem: every case is reported, not only the first that fails
		if c.got.status != c.status || problemCode(t, c.got) != c.code {
			t.Errorf("%s = %d %s, want %d %s", name, c.got.status, c.got.body, c.status, c.code)
		}
	}
}

// TestAForgottenKeyDoesNotMoveMoneyAgain: once the key store has forgotten a deposit's Idempotency-Key (after
// its 7 days), a retry with that key reaches the ledger with the same reference: 409 duplicate_external_ref
// and the money is posted once.
func TestAForgottenKeyDoesNotMoveMoneyAgain(t *testing.T) {
	w := newDepositWorld(t)
	a := w.opened(t, w.party(t, party.KYCApproved), w.savings(t, true))
	path, key := "/v1/deposit-accounts/"+a.Id.String()+"/deposits", ids.New().String()
	expectCreated(t, "deposit", postTo(t, w.ts, path, key, amountBody(1000)))
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `DELETE FROM infra.idempotency_keys WHERE key = $1`, key)
		return err
	})

	retried := postTo(t, w.ts, path, key, amountBody(1000))

	if retried.status != http.StatusConflict || problemCode(t, retried) != "duplicate_external_ref" || w.natural(t, a) != 1000 {
		t.Fatalf("retry after the key expired = %d %s with a balance of %d, want 409 duplicate_external_ref and 1000",
			retried.status, retried.body, w.natural(t, a))
	}
}

// TestARejectedMovementNamesItsAccounts (FR-40): a deposit or a transfer whose body is rejected (an unknown
// currency) is audited with the rejection and the accounts it named.
func TestARejectedMovementNamesItsAccounts(t *testing.T) {
	w := newDepositWorld(t)
	customer, savings := w.party(t, party.KYCApproved), w.savings(t, true)
	a, b := w.opened(t, customer, savings), w.opened(t, customer, savings)
	unknownCurrency := func(body string) string { return strings.Replace(body, "COP", "XXX", 1) }

	for name, c := range map[string]struct {
		path, body string
		targets    []string
	}{
		"deposit":  {"/v1/deposit-accounts/" + a.Id.String() + "/deposits", unknownCurrency(amountBody(100)), []string{a.Id.String()}},
		"transfer": {"/v1/transfers", unknownCurrency(transferBody(a, b, 100)), []string{a.Id.String(), b.Id.String()}},
	} {
		t.Run(name, func(t *testing.T) {
			resp, correlation := auditedRequest(t, w.ts, http.MethodPost, c.path, ids.New().String(), c.body)

			got := onlyRecord(t, correlation)
			if resp.status/100 != 4 || got.Result != codeOf(t, resp) || !reflect.DeepEqual(got.Targets, c.targets) {
				t.Fatalf("response %d %s; record %+v, want the rejection with targets %v", resp.status, resp.body, got.Record, c.targets)
			}
		})
	}
}

// TestALimitIsAProblemThatNamesIt (FR-24): a movement over a limit of the product is 422 limit_exceeded with
// the limit in the detail, and moves nothing.
func TestALimitIsAProblemThatNamesIt(t *testing.T) {
	w := newDepositWorld(t)
	a := w.opened(t, w.party(t, party.KYCApproved), w.limitedSavings(t, `{"per_operation": {"units": 1000}}`))

	r := w.move(t, a.Id.String(), "deposits", amountBody(1001))

	var p Problem
	if err := json.Unmarshal(r.body, &p); err != nil || r.status != http.StatusUnprocessableEntity || p.Code != "limit_exceeded" ||
		p.Detail != "The movement exceeds the product's per_operation limit of 1000 minor units." || w.natural(t, a) != 0 {
		t.Fatalf("deposit over the limit = %d %s (%v), balance %d; want 422 limit_exceeded naming per_operation and 0", r.status, r.body, err, w.natural(t, a))
	}
}

// historyPage reads the history page of query at path, which must be 200, and its entries' amounts.
func historyPage(t *testing.T, ts *httptest.Server, path, query string) (gen.AccountHistory, []int64) {
	t.Helper()
	var page gen.AccountHistory
	if status := call(t, ts, http.MethodGet, path+query, "", &page); status != http.StatusOK {
		t.Fatalf("history %s = %d, want 200", query, status)
	}
	units := make([]int64, len(page.Entries))
	for i, e := range page.Entries {
		units[i] = e.Amount.Amount
	}
	return page, units
}

// TestTheHistoryPagesOverHTTP (FR-44): after a deposit of 100,000 and a withdrawal of 60,000 with its GMF of
// 240, the account's history in pages of 2 is the GMF and the withdrawal, of one transaction, then the
// deposit, with no cursor after it; without limit, or with one of 200, it is one page of the three.
func TestTheHistoryPagesOverHTTP(t *testing.T) {
	w := newDepositWorld(t)
	a := w.opened(t, w.party(t, party.KYCApproved), w.savings(t, true))
	expectCreated(t, "deposit", w.move(t, a.Id.String(), "deposits", amountBody(10000000)))
	expectCreated(t, "withdrawal", w.move(t, a.Id.String(), "withdrawals", amountBody(6000000)))
	path, all := "/v1/deposit-accounts/"+a.Id.String()+"/transactions", []int64{24000, 6000000, -10000000}

	first, firstUnits := historyPage(t, w.ts, path, "?limit=2")
	if first.NextCursor == nil {
		t.Fatalf("first page = %+v, want a next cursor", first)
	}
	second, secondUnits := historyPage(t, w.ts, path, "?limit=2&cursor="+*first.NextCursor)
	whole, wholeUnits := historyPage(t, w.ts, path, "")
	_, largestUnits := historyPage(t, w.ts, path, "?limit=200")

	if second.NextCursor != nil || !slices.Equal(append(firstUnits, secondUnits...), all) ||
		first.Entries[0].TransactionId != first.Entries[1].TransactionId {
		t.Fatalf("pages = %+v and %+v, want the withdrawal's GMF and amount, then the deposit, and no cursor after it", first, second)
	}
	if whole.NextCursor != nil || !slices.Equal(wholeUnits, all) || !slices.Equal(largestUnits, all) {
		t.Fatalf("without limit = %+v and with 200 %v, want one page of the three", whole, largestUnits)
	}
}

// TestAWrongPageIsAProblem (FR-44): a limit outside 1 to 200 or a cursor of nowhere is 400 invalid_request,
// and the history of an account of nowhere is 404.
func TestAWrongPageIsAProblem(t *testing.T) {
	w := newDepositWorld(t)
	a := w.opened(t, w.party(t, party.KYCApproved), w.savings(t, true))
	path := "/v1/deposit-accounts/" + a.Id.String() + "/transactions"
	cases := map[string]struct {
		path   string
		status int
		code   string
	}{
		"limit 0":             {path + "?limit=0", http.StatusBadRequest, "invalid_request"},
		"limit 201":           {path + "?limit=201", http.StatusBadRequest, "invalid_request"},
		"a cursor of nowhere": {path + "?cursor=nowhere", http.StatusBadRequest, "invalid_request"},
		// two varints, but posted 9,500 years before 1970: PostgreSQL could not even compare it
		"a cursor out of time":  {path + "?cursor=___viaOa6KkIAg", http.StatusBadRequest, "invalid_request"},
		"an account of nowhere": {"/v1/deposit-accounts/" + ids.New().String() + "/transactions", http.StatusNotFound, "not_found"},
	}
	for name, tc := range cases {
		var problem Problem
		if status := call(t, w.ts, http.MethodGet, tc.path, "", &problem); status != tc.status || problem.Code != tc.code {
			t.Errorf("%s = %d %+v, want %d %s", name, status, problem, tc.status, tc.code)
		}
	}
}
