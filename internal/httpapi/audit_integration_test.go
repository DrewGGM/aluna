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
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/audit"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// auditedRow is an audit record with the xmin of its row: the transaction that wrote it.
type auditedRow struct {
	audit.Record
	xmin string
}

// auditedRequest makes a request with its own X-Request-Id and returns the response and its correlation.
func auditedRequest(t *testing.T, ts *httptest.Server, method, path, key, body string) (response, string) {
	t.Helper()
	correlation := ids.New().String()
	req, err := http.NewRequestWithContext(t.Context(), method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerRequestID, correlation)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // close in a test
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{status: resp.StatusCode, body: b}, correlation
}

// auditOf returns, as the owner (aluna_app only inserts), the records of the correlation in order. after is
// read with exact numbers.
func auditOf(t *testing.T, correlation string) []auditedRow {
	t.Helper()
	var rows []auditedRow
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		res, err := tx.Query(t.Context(), `SELECT actor, role, channel, ip, command, targets, result, after::text, xmin::text
			FROM audit.records WHERE correlation_id = $1 ORDER BY seq`, correlation)
		if err != nil {
			return err
		}
		rows, err = pgx.CollectRows(res, func(row pgx.CollectableRow) (auditedRow, error) {
			var r auditedRow
			var after *string
			if err := row.Scan(&r.Actor, &r.Role, &r.Channel, &r.IP, &r.Command, &r.Targets, &r.Result, &after, &r.xmin); err != nil {
				return r, err
			}
			if after != nil {
				r.After = decodeExact(t, []byte(*after))
			}
			return r, nil
		})
		return err
	})
	return rows
}

func decodeExact(t *testing.T, b []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// textOf is, as the owner, the single value of query as text.
func textOf(t *testing.T, query string, args ...any) string {
	t.Helper()
	var v string
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error { return tx.QueryRow(t.Context(), query, args...).Scan(&v) })
	return v
}

func onlyRecord(t *testing.T, correlation string) auditedRow {
	t.Helper()
	rows := auditOf(t, correlation)
	if len(rows) != 1 {
		t.Fatalf("%d audit records for %s, want 1", len(rows), correlation)
	}
	return rows[0]
}

// codeOf is the problem code of r.
func codeOf(t *testing.T, r response) string {
	t.Helper()
	var p Problem
	if err := json.Unmarshal(r.body, &p); err != nil || p.Code == "" {
		t.Fatalf("response %d without problem: %s", r.status, r.body)
	}
	return p.Code
}

// TestACommandIsAuditedInItsTransaction (FR-40): a successful POST leaves its record in the same transaction
// as its effect: who, from where, the command, the IDs it touched (the transaction and its accounts) and a
// summary without the client's free text, with exact amounts even above 2^53.
func TestACommandIsAuditedInItsTransaction(t *testing.T) {
	ts, cash, deposit := newAccounts(t)
	const big = 9007199254740993 // 2^53 + 1: a float64 cannot represent it

	resp, correlation := auditedRequest(t, ts, http.MethodPost, "/v1/transactions", ids.New().String(), postingBody(t, cash, deposit, big, big, ""))

	if resp.status != http.StatusCreated {
		t.Fatalf("posting = %d %s", resp.status, resp.body)
	}
	body := decodeExact(t, resp.body)
	id, _ := body["id"].(string)
	got := onlyRecord(t, correlation)
	want := audit.Record{
		Actor: testSubject, Role: testRole, Channel: auditChannel, IP: "127.0.0.1", Command: "POST /v1/transactions",
		Targets: slices.Sorted(slices.Values([]string{id, cash, deposit})), Result: resultOK, After: got.After,
	}
	if !reflect.DeepEqual(got.Record, want) {
		t.Fatalf("record = %+v, want %+v", got.Record, want)
	}
	checkPostingSummary(t, got.After, body)
	if tx := textOf(t, `SELECT xmin::text FROM ledger.transactions WHERE id = $1`, id); got.xmin != tx {
		t.Fatalf("transaction %s wrote the record and transaction %s the posting", got.xmin, tx)
	}
}

// checkPostingSummary checks that a posting's after has its exact id and entries, and none of the client's
// free text.
func checkPostingSummary(t *testing.T, after, body map[string]any) {
	t.Helper()
	for _, key := range []string{"code", "source", "external_ref"} {
		if _, ok := after[key]; ok {
			t.Errorf("after carries %q, client free text", key)
		}
	}
	if after["id"] != body["id"] || !reflect.DeepEqual(after["entries"], body["entries"]) {
		t.Errorf("after = %v, want the id and entries of the response %v", after, body)
	}
}

// TestABusinessRejectionIsAuditedInItsTransaction: a business rejection inside the command leaves its record
// with the problem code, in the transaction that stores the Idempotency-Key response; a retry with the same
// key is recorded as a replay.
func TestABusinessRejectionIsAuditedInItsTransaction(t *testing.T) {
	ts, cash, deposit := newAccounts(t)
	key := ids.New().String()
	overdraft := postingBody(t, deposit, cash, 100, 100, "") // the empty deposit cannot pay

	resp, correlation := auditedRequest(t, ts, http.MethodPost, "/v1/transactions", key, overdraft)
	_, replayed := auditedRequest(t, ts, http.MethodPost, "/v1/transactions", key, overdraft)

	got := onlyRecord(t, correlation)
	if code := codeOf(t, resp); got.Result != code || got.After != nil || len(got.Targets) != 0 {
		t.Fatalf("record %+v, want the rejection %s without after or targets", got.Record, code)
	}
	if stored := textOf(t, `SELECT xmin::text FROM infra.idempotency_keys WHERE key = $1`, key); got.xmin != stored {
		t.Fatalf("transaction %s wrote the record and transaction %s the stored response", got.xmin, stored)
	}
	if r := onlyRecord(t, replayed); r.Result != resultReplay {
		t.Fatalf("the retry was recorded as %q, want %q", r.Result, resultReplay)
	}
}

// TestARejectionBeforeTheCommandIsAudited: what is rejected before the command's transaction (an unknown
// currency, an Idempotency-Key reused with another body) is also recorded, with its code, in a separate
// transaction.
func TestARejectionBeforeTheCommandIsAudited(t *testing.T) {
	ts, cash, deposit := newAccounts(t)
	key := ids.New().String()
	if first, _ := auditedRequest(t, ts, http.MethodPost, "/v1/transactions", key, postingBody(t, cash, deposit, 100, 100, "")); first.status != http.StatusCreated {
		t.Fatalf("first posting = %d %s", first.status, first.body)
	}
	cases := map[string]func() (response, string){
		"unknown currency": func() (response, string) {
			return auditedRequest(t, ts, http.MethodPost, "/v1/ledger-accounts", "", `{"code":"acct-x","kind":"asset","currency":"XXX","balance_mode":"sync"}`)
		},
		"reused key": func() (response, string) {
			return auditedRequest(t, ts, http.MethodPost, "/v1/transactions", key, postingBody(t, cash, deposit, 200, 200, ""))
		},
	}
	for name, send := range cases {
		t.Run(name, func(t *testing.T) {
			resp, correlation := send()

			if got := onlyRecord(t, correlation); resp.status/100 != 4 || got.Result != codeOf(t, resp) {
				t.Fatalf("response %d %s; recorded as %q", resp.status, resp.body, got.Result)
			}
		})
	}
}

type failingEvents struct{}

var errEventsDown = errors.New("the outbox does not answer")

func (failingEvents) Emit(context.Context, postgres.Tx, outbox.Event) error { return errEventsDown }

// TestAFailedCommandIsAuditedApart: a command that ends in 500 rolls back its transaction, with its effect and
// its response; the record is kept anyway, with the problem code, in a separate transaction.
func TestAFailedCommandIsAuditedApart(t *testing.T) {
	ts := apiServerWith(t, ids.New(), failingEvents{})
	cash, deposit := createAccount(t, ts, "asset"), createAccount(t, ts, "liability")
	key := ids.New().String()

	resp, correlation := auditedRequest(t, ts, http.MethodPost, "/v1/transactions", key, postingBody(t, cash, deposit, 100, 100, ""))

	got := onlyRecord(t, correlation)
	if resp.status != http.StatusInternalServerError || got.Result != codeOf(t, resp) || got.Command != "POST /v1/transactions" {
		t.Fatalf("response %d; record %+v, want a 500 recorded with its code", resp.status, got.Record)
	}
	if stored := textOf(t, `SELECT count(*)::text FROM infra.idempotency_keys WHERE key = $1`, key); stored != "0" {
		t.Fatalf("the 500 response was stored (%s rows)", stored)
	}
}

// TestAPanickingCommandIsAudited: auditing sits outside recovery: a command that panics is recorded with the
// 500 that withRecovery answers.
func TestAPanickingCommandIsAudited(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := ServerConfig{Audit: &CommandAudit{Records: audit.New(), UoW: postgres.NewUnitOfWork(testPool)}}
	routes := func(mux *http.ServeMux) {
		mux.HandleFunc("POST /v1/panic", func(http.ResponseWriter, *http.Request) { panic("fails on purpose") })
	}
	ts := httptest.NewServer(NewServer(cfg, routes, log).Handler)
	t.Cleanup(ts.Close)

	resp, correlation := auditedRequest(t, ts, http.MethodPost, "/v1/panic", "", "{}")

	if got := onlyRecord(t, correlation); resp.status != http.StatusInternalServerError || got.Result != codeOf(t, resp) || got.Command != "POST /v1/panic" {
		t.Fatalf("response %d; record %+v", resp.status, got.Record)
	}
}

// TestCreatingAnAccountIsAudited: creating an account, even without Idempotency-Key, leaves its record in its
// transaction; repeating the code is a business rejection that is also recorded, with its code.
func TestCreatingAnAccountIsAudited(t *testing.T) {
	ts := apiServer(t, ids.New())
	body := `{"code":"acct-` + ids.New().String() + `","kind":"asset","currency":"COP","balance_mode":"sync"}`

	created, first := auditedRequest(t, ts, http.MethodPost, "/v1/ledger-accounts", "", body)
	repeated, second := auditedRequest(t, ts, http.MethodPost, "/v1/ledger-accounts", "", body)

	got := onlyRecord(t, first)
	id, _ := decodeExact(t, created.body)["id"].(string)
	if created.status != http.StatusCreated || got.Result != resultOK || !reflect.DeepEqual(got.Targets, []string{id}) {
		t.Fatalf("response %d; record %+v", created.status, got.Record)
	}
	if account := textOf(t, `SELECT xmin::text FROM ledger.accounts WHERE id = $1`, id); got.xmin != account {
		t.Fatalf("transaction %s wrote the record and transaction %s the account", got.xmin, account)
	}
	if r := onlyRecord(t, second); r.Result != codeOf(t, repeated) {
		t.Fatalf("repeated code = %d, recorded as %q", repeated.status, r.Result)
	}
}

// TestACaptureIsAuditedWithTheHoldAndItsTransaction: the command is the route with its parameter and the
// targets include the hold and the transaction that posted it.
func TestACaptureIsAuditedWithTheHoldAndItsTransaction(t *testing.T) {
	ts, _, hold := depositWithHold(t)

	resp, correlation := auditedRequest(t, ts, http.MethodPost, "/v1/holds/"+hold.Id.String()+"/capture", ids.New().String(), captureBody(200))

	got := onlyRecord(t, correlation)
	posted, _ := decodeExact(t, resp.body)["transaction"].(map[string]any)
	txID, _ := posted["id"].(string)
	if resp.status != http.StatusOK || got.Command != "POST /v1/holds/{id}/capture" || txID == "" ||
		!slices.Contains(got.Targets, hold.Id.String()) || !slices.Contains(got.Targets, txID) {
		t.Fatalf("response %d; record %s with targets %v, want the hold and %s", resp.status, got.Command, got.Targets, txID)
	}
}

// TestReadsAreNotAudited: a GET is not a command and leaves no record.
func TestReadsAreNotAudited(t *testing.T) {
	ts, cash, _ := newAccounts(t)

	resp, correlation := auditedRequest(t, ts, http.MethodGet, "/v1/ledger-accounts/"+cash, "", "")

	if rows := auditOf(t, correlation); resp.status != http.StatusOK || len(rows) != 0 {
		t.Fatalf("GET = %d and left %d audit records", resp.status, len(rows))
	}
}

// TestAnUncleanPathIsAuditedOnce: the mux redirects /v1//transactions to the clean path; the command is
// recorded once, in the request that runs it, and the redirect leaves no error record.
func TestAnUncleanPathIsAuditedOnce(t *testing.T) {
	ts, cash, deposit := newAccounts(t)

	resp, correlation := auditedRequest(t, ts, http.MethodPost, "/v1//transactions", ids.New().String(), postingBody(t, cash, deposit, 100, 100, ""))

	if got := onlyRecord(t, correlation); resp.status != http.StatusCreated || got.Result != resultOK {
		t.Fatalf("response %d; record %+v", resp.status, got.Record)
	}
}

// TestARejectedHoldCommandNamesTheHold: a rejected command on a hold leaves as target the hold of its path,
// whether inside the tx (a second release) or before it (an unknown currency, an invalid key).
func TestARejectedHoldCommandNamesTheHold(t *testing.T) {
	ts, _, hold := depositWithHold(t)
	base := "/v1/holds/" + hold.Id.String()
	if first, _ := auditedRequest(t, ts, http.MethodPost, base+"/release", ids.New().String(), ""); first.status != http.StatusOK {
		t.Fatalf("first release = %d %s", first.status, first.body)
	}
	cases := map[string]func() (response, string){
		"repeated release": func() (response, string) {
			return auditedRequest(t, ts, http.MethodPost, base+"/release", ids.New().String(), "")
		},
		"unknown currency": func() (response, string) {
			return auditedRequest(t, ts, http.MethodPost, base+"/capture", ids.New().String(), strings.Replace(captureBody(10), "COP", "XXX", 1))
		},
		"invalid key": func() (response, string) {
			return auditedRequest(t, ts, http.MethodPost, base+"/release", strings.Repeat("k", maxIdempotencyKey+1), "")
		},
	}
	for name, send := range cases {
		t.Run(name, func(t *testing.T) {
			resp, correlation := send()

			got := onlyRecord(t, correlation)
			if resp.status/100 != 4 || got.Result != codeOf(t, resp) || !reflect.DeepEqual(got.Targets, []string{hold.Id.String()}) {
				t.Fatalf("response %d %s; record %+v, want the rejection with the hold as target", resp.status, resp.body, got.Record)
			}
		})
	}
}

// TestAForbiddenCommandIsAudited: a POST from a role without the permission gets 403 and is recorded with its
// actor and role, and the problem code; also through a non-canonical path, which the mux redirects.
func TestAForbiddenCommandIsAudited(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := ServerConfig{
		Audit:    &CommandAudit{Records: audit.New(), UoW: postgres.NewUnitOfWork(testPool)},
		Verifier: rolesVerifier{}, Authorizer: testAuthorizer(t),
	}
	handler := NewServer(cfg, Routes(API{}, log), log).Handler
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Authorization", "Bearer auditor")
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)

	// the second, after the mux redirect; the third is canonical for the mux (it looks at the escaped path)
	for _, path := range []string{"/v1/ledger-accounts", "/v1//ledger-accounts", "/v1/holds/%2E%2E/capture"} {
		resp, correlation := auditedRequest(t, ts, http.MethodPost, path, "", `{}`)

		got := onlyRecord(t, correlation)
		if resp.status != http.StatusForbidden || got.Result != "forbidden" || got.Actor != "subject-1" || got.Role != "auditor" {
			t.Fatalf("POST %s: response %d; record %+v, want the auditor's 403", path, resp.status, got.Record)
		}
	}
}

// TestAnUnauthenticatedCommandIsAudited: a POST without a token gets 401 and is recorded, with no known actor,
// with the problem code.
func TestAnUnauthenticatedCommandIsAudited(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := ServerConfig{
		Audit:    &CommandAudit{Records: audit.New(), UoW: postgres.NewUnitOfWork(testPool)},
		Verifier: entityVerifier{Subject: testSubject, LegalEntityID: ids.New()},
	}
	ts := httptest.NewServer(NewServer(cfg, Routes(API{}, log), log).Handler)
	t.Cleanup(ts.Close)

	resp, correlation := auditedRequest(t, ts, http.MethodPost, "/v1/ledger-accounts", "", `{}`)

	got := onlyRecord(t, correlation)
	if resp.status != http.StatusUnauthorized || got.Result != "unauthenticated" || got.Actor != auditActor || got.Role != auditRole {
		t.Fatalf("response %d; record %+v, want an anonymous caller's 401", resp.status, got.Record)
	}
}
