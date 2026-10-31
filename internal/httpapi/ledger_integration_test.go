//go:build integration

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DrewGGM/aluna/config"
	"github.com/DrewGGM/aluna/internal/access"
	"github.com/DrewGGM/aluna/internal/approvals"
	"github.com/DrewGGM/aluna/internal/audit"
	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/party"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, &testPool)) }

// apiServer starts the full API (middleware, auditing, generated code and handlers) of the entity entity.
func apiServer(t *testing.T, entity ids.ID) *httptest.Server {
	t.Helper()
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	return apiServerWith(t, entity, events)
}

// apiServerWith is apiServer with the ledger events in events.
func apiServerWith(t *testing.T, entity ids.ID, events ledger.Events) *httptest.Server {
	t.Helper()
	return apiServerAs(t, entity, events, testSubject)
}

// apiServerAs is the full API with the session of the entity's operator subject: every request carries the
// test token, which entityVerifier accepts as that operator (token verification is tested in auth_test.go).
// Reversals go through maker-checker as in aluna api.
func apiServerAs(t *testing.T, entity ids.ID, events ledger.Events, subject string) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	uow, keys := postgres.NewUnitOfWork(testPool), idempotency.NewStore(testPool)
	policy, err := access.ParsePolicy(config.Roles)
	if err != nil {
		t.Fatal(err)
	}
	l, requests := ledger.New(calendar.New(), events), approvals.New(policy)
	api := API{
		LedgerHandlers:   NewLedgerHandlers(l, requests, uow, keys, testPool),
		ApprovalHandlers: NewApprovalHandlers(requests, uow, keys, testPool),
		PartyHandlers:    NewPartyHandlers(party.New(events), uow, testPool),
	}
	cfg := ServerConfig{
		Audit:      &CommandAudit{Records: audit.New(), UoW: uow},
		Verifier:   entityVerifier{Subject: subject, LegalEntityID: entity, Roles: []string{testRole}},
		Authorizer: testAuthorizer(t),
	}
	handler := NewServer(cfg, Routes(api, log), log).Handler
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+testToken) // replaces whatever the test brings
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

const (
	testToken   = "test-token" // not a credential: entityVerifier only accepts this text
	testSubject = "test-operator"
	testRole    = "operator"
)

var errUnknownToken = errors.New("unknown token")

// entityVerifier accepts testToken as its principal.
type entityVerifier access.Principal

func (v entityVerifier) Verify(_ context.Context, raw string) (access.Principal, error) {
	if raw != testToken {
		return access.Principal{}, errUnknownToken
	}
	return access.Principal(v), nil
}

// call makes a JSON request, decodes the response into out (if not nil) and returns the status.
func call(t *testing.T, ts *httptest.Server, method, path, body string, out any) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", ids.New().String())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // close in a test
	if _, isProblem := out.(*Problem); isProblem && resp.Header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("%s %s = %d with Content-Type %q, want application/problem+json", method, path, resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s = %d: invalid body: %v", method, path, resp.StatusCode, err)
		}
	}
	return resp.StatusCode
}

func createAccount(t *testing.T, ts *httptest.Server, kind string) string {
	t.Helper()
	var account gen.LedgerAccount
	status := call(t, ts, http.MethodPost, "/v1/ledger-accounts",
		`{"code":"acct-`+ids.New().String()+`","kind":"`+kind+`","currency":"COP","balance_mode":"sync"}`, &account)
	if status != http.StatusCreated {
		t.Fatalf("create account = %d", status)
	}
	return account.Id.String()
}

// postingValueDate is the value date of postingBody's postings.
const postingValueDate = "2026-10-26"

// postingBody builds the body of a posting in COP: debitUnits to the debit and creditUnits (positive) to the
// credit. ref repeats the external reference if not empty.
func postingBody(t *testing.T, debit, credit string, debitUnits, creditUnits int64, ref string) string {
	t.Helper()
	if ref == "" {
		ref = ids.New().String()
	}
	b, err := json.Marshal(map[string]any{
		"code": "deposit", "value_date": postingValueDate, "business_date": postingValueDate, "source": "test",
		"external_ref": ref,
		"entries": []map[string]any{
			{"account_id": debit, "amount": map[string]any{"amount": debitUnits, "currency": "COP"}},
			{"account_id": credit, "amount": map[string]any{"amount": -creditUnits, "currency": "COP"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLedgerOverHTTP(t *testing.T) {
	ts := apiServer(t, ids.New())
	cash, deposit := createAccount(t, ts, "asset"), createAccount(t, ts, "liability")

	var posted gen.Transaction
	status := call(t, ts, http.MethodPost, "/v1/transactions", postingBody(t, cash, deposit, 10_000_000, 10_000_000, ""), &posted)

	if status != http.StatusCreated {
		t.Fatalf("post = %d", status)
	}
	var read gen.Transaction
	if status := call(t, ts, http.MethodGet, "/v1/transactions/"+posted.Id.String(), "", &read); status != http.StatusOK || read.ExternalRef != posted.ExternalRef || len(read.Entries) != 2 {
		t.Fatalf("read transaction = %d %+v", status, read)
	}
	var balance gen.Balance
	call(t, ts, http.MethodGet, "/v1/ledger-accounts/"+deposit+"/balance", "", &balance)
	if balance.Posted.Amount != -10_000_000 || balance.Natural.Amount != 10_000_000 || balance.Natural.Currency != "COP" {
		t.Fatalf("deposit balance = %+v: want signed -10,000,000 and natural 10,000,000 COP", balance)
	}
	var account gen.LedgerAccount
	if status := call(t, ts, http.MethodGet, "/v1/ledger-accounts/"+cash, "", &account); status != http.StatusOK || account.Kind != "asset" {
		t.Fatalf("read account = %d %+v", status, account)
	}
}

func TestLedgerErrorsAreProblems(t *testing.T) {
	ts := apiServer(t, ids.New())
	cash, deposit := createAccount(t, ts, "asset"), createAccount(t, ts, "liability")
	repeated := ids.New().String()
	if status := call(t, ts, http.MethodPost, "/v1/transactions", postingBody(t, cash, deposit, 100, 100, repeated), nil); status != http.StatusCreated {
		t.Fatalf("first posting = %d", status)
	}
	cases := map[string]struct {
		method, path, body string
		status             int
		code               string
	}{
		"insufficient funds":      {http.MethodPost, "/v1/transactions", postingBody(t, deposit, cash, 101, 101, ""), 422, "insufficient_funds"},
		"unbalanced":              {http.MethodPost, "/v1/transactions", postingBody(t, cash, deposit, 100, 99, ""), 422, "unbalanced_transaction"},
		"repeated reference":      {http.MethodPost, "/v1/transactions", postingBody(t, cash, deposit, 100, 100, repeated), 409, "duplicate_external_ref"},
		"unknown currency":        {http.MethodPost, "/v1/ledger-accounts", `{"code":"x","kind":"asset","currency":"XYZ","balance_mode":"sync"}`, 422, "unknown_currency"},
		"nonexistent account":     {http.MethodGet, "/v1/ledger-accounts/" + ids.New().String(), "", 404, "not_found"},
		"nonexistent transaction": {http.MethodGet, "/v1/transactions/" + ids.New().String(), "", 404, "not_found"},
		"id that is not a uuid":   {http.MethodGet, "/v1/ledger-accounts/123", "", 400, "invalid_request"},
		"broken JSON":             {http.MethodPost, "/v1/transactions", `{"code":`, 400, "invalid_request"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var p Problem
			status := call(t, ts, tc.method, tc.path, tc.body, &p)

			if status != tc.status || p.Code != tc.code {
				t.Fatalf("= %d %+v, want %d %s", status, p, tc.status, tc.code)
			}
			if strings.Contains(p.Detail, "pgx") || strings.Contains(p.Detail, "ledger.") || strings.Contains(p.Detail, "SQLSTATE") {
				t.Fatalf("the problem leaks internal details: %q", p.Detail)
			}
		})
	}
}

// TestOtherEntitiesResourcesAreNotVisible: one entity's deployment does not see another's accounts or
// transactions even if they share the database.
func TestOtherEntitiesResourcesAreNotVisible(t *testing.T) {
	mine, theirs := apiServer(t, ids.New()), apiServer(t, ids.New())
	cash, deposit := createAccount(t, theirs, "asset"), createAccount(t, theirs, "liability")
	var posted gen.Transaction
	if status := call(t, theirs, http.MethodPost, "/v1/transactions", postingBody(t, cash, deposit, 100, 100, ""), &posted); status != http.StatusCreated {
		t.Fatalf("the other entity's posting = %d", status)
	}

	for _, path := range []string{"/v1/ledger-accounts/" + cash, "/v1/ledger-accounts/" + cash + "/balance", "/v1/transactions/" + posted.Id.String()} {
		var p Problem
		if status := call(t, mine, http.MethodGet, path, "", &p); status != http.StatusNotFound || p.Code != "not_found" {
			t.Errorf("GET %s from another entity = %d %+v, want 404", path, status, p)
		}
	}
	if status := call(t, mine, http.MethodPost, "/v1/transactions", postingBody(t, cash, deposit, 1, 1, ""), nil); status != http.StatusNotFound {
		t.Errorf("posting on another entity's accounts = %d, want 404", status)
	}
}

// TestBigBodyWithoutLengthIs413: withBodyLimit cuts a body without Content-Length and the generated code
// answers it as 413.
func TestBigBodyWithoutLengthIs413(t *testing.T) {
	ts := apiServer(t, ids.New())
	huge := `{"code":"` + strings.Repeat("x", maxBodyBytes) + `"}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/v1/ledger-accounts", io.NopCloser(bytes.NewReader([]byte(huge))))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1 // chunked

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // close in a test

	var p Problem
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil || resp.StatusCode != http.StatusRequestEntityTooLarge || p.Code != "body_too_large" {
		t.Fatalf("= %d %+v (%v), want 413 body_too_large", resp.StatusCode, p, err)
	}
}

// TestTheBitemporalBalanceOverHTTP (FR-8): with cutoffs, the balance is summed from the entries and carries
// the cutoffs it was computed with, without pending amounts; the posting is not there as of the previous
// day's value date nor as known an hour ago.
func TestTheBitemporalBalanceOverHTTP(t *testing.T) {
	ts := apiServer(t, ids.New())
	cash, deposit := createAccount(t, ts, "asset"), createAccount(t, ts, "liability")
	if status := call(t, ts, http.MethodPost, "/v1/transactions", postingBody(t, cash, deposit, 500, 500, ""), nil); status != http.StatusCreated {
		t.Fatalf("post = %d", status)
	}
	anHourAgo := url.QueryEscape(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339))
	cases := map[string]struct {
		query   string
		natural int64
	}{
		"at its value date": {"as_of_value_date=" + postingValueDate, 500},
		"the day before":    {"as_of_value_date=" + dayBefore(t, postingValueDate), 0},
		"as known 1 h ago":  {"as_known_at=" + anHourAgo, 0},
	}
	for name, tc := range cases {
		b := balanceAt(t, ts, deposit, "?"+tc.query)
		if b.Natural.Amount != tc.natural || echoedCut(b) != tc.query || b.PendingDebits != nil || b.PendingCredits != nil || b.Version != nil {
			t.Errorf("%s = %+v, want natural %d with its cutoff and without pending amounts", name, b, tc.natural)
		}
	}
}

// TestWithoutCutsTheBalanceIsTheMaterialized: without cutoffs, or with the empty parameters a URL template
// leaves, the balance is the materialized one, with its version.
func TestWithoutCutsTheBalanceIsTheMaterialized(t *testing.T) {
	ts := apiServer(t, ids.New())
	cash, deposit := createAccount(t, ts, "asset"), createAccount(t, ts, "liability")
	if status := call(t, ts, http.MethodPost, "/v1/transactions", postingBody(t, cash, deposit, 500, 500, ""), nil); status != http.StatusCreated {
		t.Fatalf("post = %d", status)
	}
	for _, query := range []string{"", "?as_of_value_date=&as_known_at="} {
		if b := balanceAt(t, ts, deposit, query); b.Natural.Amount != 500 || b.Version == nil || b.AsKnownAt != nil || b.AsOfValueDate != nil {
			t.Errorf("balance%s = %+v, want the materialized one with its version", query, b)
		}
	}
}

func dayBefore(t *testing.T, date string) string {
	t.Helper()
	d, err := time.Parse(time.DateOnly, date)
	if err != nil {
		t.Fatal(err)
	}
	return d.AddDate(0, 0, -1).Format(time.DateOnly)
}

// echoedCut is the cutoff the bitemporal balance returned, written as the parameter that requested it.
func echoedCut(b gen.Balance) string {
	switch {
	case b.AsOfValueDate != nil && b.AsKnownAt == nil:
		return "as_of_value_date=" + b.AsOfValueDate.String()
	case b.AsKnownAt != nil && b.AsOfValueDate == nil:
		return "as_known_at=" + url.QueryEscape(b.AsKnownAt.UTC().Format(time.RFC3339))
	}
	return ""
}

func balanceAt(t *testing.T, ts *httptest.Server, account, query string) gen.Balance {
	t.Helper()
	var b gen.Balance
	if status := call(t, ts, http.MethodGet, "/v1/ledger-accounts/"+account+"/balance"+query, "", &b); status != http.StatusOK {
		t.Fatalf("balance%s = %d", query, status)
	}
	return b
}

// TestAClosedPeriodIsAConflict (FR-15): posting with a business date in a closed period is 409
// period_closed.
func TestAClosedPeriodIsAConflict(t *testing.T) {
	entity := ids.New()
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error { // entities and closes do not belong to the application
		if _, err := tx.Exec(t.Context(), `INSERT INTO calendar.legal_entities (id, name, business_date, functional_currency)
			VALUES ($1, 'Entity', '2025-07-07', 'COP')`, entity); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), `INSERT INTO calendar.periods (legal_entity_id, period, status, closed_at, closed_by)
			VALUES ($1, '2025-06', 'closed', clock_timestamp(), 'accountant')`, entity)
		return err
	})
	ts := apiServer(t, entity)
	cash, deposit := createAccount(t, ts, "asset"), createAccount(t, ts, "liability")
	body := strings.Replace(postingBody(t, cash, deposit, 100, 100, ""), `"business_date":"`+postingValueDate, `"business_date":"2025-06-30`, 1)

	r := postWithKey(t, ts, ids.New().String(), body)

	if r.status != http.StatusConflict || problemCode(t, r) != "period_closed" {
		t.Fatalf("posting in June = %d %s, want 409 period_closed", r.status, r.body)
	}
}

// TestTheZeroBusinessDateIsInvalid: through the API the business date is the caller's; 0001-01-01 is not
// taken as absent (the ledger would use the entity's) but as invalid.
func TestTheZeroBusinessDateIsInvalid(t *testing.T) {
	ts := apiServer(t, ids.New())
	cash, deposit := createAccount(t, ts, "asset"), createAccount(t, ts, "liability")
	body := strings.Replace(postingBody(t, cash, deposit, 100, 100, ""), `"business_date":"`+postingValueDate, `"business_date":"0001-01-01`, 1)

	r := postWithKey(t, ts, ids.New().String(), body)

	if r.status != http.StatusUnprocessableEntity || problemCode(t, r) != "invalid_posting" {
		t.Fatalf("posting with a zero business date = %d %s, want 422 invalid_posting", r.status, r.body)
	}
}
