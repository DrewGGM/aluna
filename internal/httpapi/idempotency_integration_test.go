//go:build integration

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/DrewGGM/aluna/internal/access"
	"github.com/DrewGGM/aluna/internal/audit"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// response is what the client sees of a response.
type response struct {
	status      int
	contentType string
	body        []byte
}

// send does POST path with body and the Idempotency-Key header (omitted if key is nil). It does not call
// t.Fatal: it can be used from other goroutines.
func send(ctx context.Context, ts *httptest.Server, path string, key *string, body string) (response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+path, strings.NewReader(body))
	if err != nil {
		return response{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != nil {
		req.Header.Set("Idempotency-Key", *key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return response{}, err
	}
	defer resp.Body.Close() //nolint:errcheck // close in a test
	b, err := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: b}, err
}

// postWithKey does POST /v1/transactions with the Idempotency-Key key.
func postWithKey(t *testing.T, ts *httptest.Server, key, body string) response {
	t.Helper()
	return postTo(t, ts, "/v1/transactions", key, body)
}

func postTo(t *testing.T, ts *httptest.Server, path, key, body string) response {
	t.Helper()
	r, err := send(t.Context(), ts, path, &key, body)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// concurrently does the same POST /v1/transactions n times at once and returns the responses.
func concurrently(t *testing.T, n int, ts *httptest.Server, key, body string) []response {
	t.Helper()
	responses, errs := make([]response, n), make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { responses[i], errs[i] = send(t.Context(), ts, "/v1/transactions", &key, body) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	return responses
}

// allTheSame checks that every response is identical to want.
func allTheSame(t *testing.T, responses []response, want response) {
	t.Helper()
	for i, r := range responses {
		if r.status != want.status || !bytes.Equal(r.body, want.body) {
			t.Errorf("response %d = %d %s, want %d %s", i, r.status, r.body, want.status, want.body)
		}
	}
}

func problemCode(t *testing.T, r response) string {
	t.Helper()
	var p Problem
	if err := json.Unmarshal(r.body, &p); err != nil || r.contentType != "application/problem+json" {
		t.Fatalf("response %d is not a problem: %s (%v)", r.status, r.body, err)
	}
	return p.Code
}

func balanceOf(t *testing.T, ts *httptest.Server, account string) int64 {
	t.Helper()
	var b struct{ Posted struct{ Amount int64 } }
	if status := call(t, ts, http.MethodGet, "/v1/ledger-accounts/"+account+"/balance", "", &b); status != http.StatusOK {
		t.Fatalf("balance = %d", status)
	}
	return b.Posted.Amount
}

// newAccounts creates a cash account and a deposit in a new entity.
func newAccounts(t *testing.T) (ts *httptest.Server, cash, deposit string) {
	t.Helper()
	ts = apiServer(t, ids.New())
	return ts, createAccount(t, ts, "asset"), createAccount(t, ts, "liability")
}

func TestPostTransactionRequiresAValidIdempotencyKey(t *testing.T) {
	ts, cash, deposit := newAccounts(t)
	body := postingBody(t, cash, deposit, 100, 100, "")
	empty, long, notUTF8 := "", strings.Repeat("k", 256), "\xff\xfekey"
	cases := map[string]struct {
		key  *string
		code string
	}{
		"without the header": {nil, "idempotency_key_required"},
		"empty":              {&empty, "idempotency_key_required"},
		"of 256 characters":  {&long, "invalid_request"},
		"not UTF-8":          {&notUTF8, "invalid_request"},
	}
	for name, tc := range cases {
		r, err := send(t.Context(), ts, "/v1/transactions", tc.key, body)
		if err != nil || r.status != http.StatusBadRequest || problemCode(t, r) != tc.code {
			t.Errorf("key %s = %d %s (%v), want 400 %s", name, r.status, r.body, err, tc.code)
		}
	}
	if r := postWithKey(t, ts, strings.Repeat("\u00e9", 255), body); r.status != http.StatusCreated {
		t.Errorf("255 two-byte characters = %d %s, want 201: the limit is in characters", r.status, r.body)
	}
	if got := balanceOf(t, ts, cash); got != 100 {
		t.Errorf("balance = %d, want 100: rejected requests do not post", got)
	}
}

// TestReplayReturnsTheOriginalResponse: the retry receives the same status and the same body, byte for byte,
// even if the JSON arrives with another format, and the money moves only once.
func TestReplayReturnsTheOriginalResponse(t *testing.T) {
	ts, cash, deposit := newAccounts(t)
	key, body := ids.New().String(), postingBody(t, cash, deposit, 100, 100, "")
	first := postWithKey(t, ts, key, body)
	if first.status != http.StatusCreated || first.contentType != "application/json" {
		t.Fatalf("first POST = %d %s", first.status, first.body)
	}
	var reformatted bytes.Buffer
	if err := json.Indent(&reformatted, []byte(body), "", "    "); err != nil {
		t.Fatal(err)
	}

	allTheSame(t, []response{postWithKey(t, ts, key, body), postWithKey(t, ts, key, reformatted.String())}, first)

	if got := balanceOf(t, ts, cash); got != 100 {
		t.Errorf("balance = %d, want 100: the replay must not post again", got)
	}
}

func TestAnotherPayloadWithTheSameKeyIsRejected(t *testing.T) {
	ts, cash, deposit := newAccounts(t)
	key, ref := ids.New().String(), ids.New().String()
	if r := postWithKey(t, ts, key, postingBody(t, cash, deposit, 100, 100, ref)); r.status != http.StatusCreated {
		t.Fatalf("first POST = %d %s", r.status, r.body)
	}

	r := postWithKey(t, ts, key, postingBody(t, cash, deposit, 200, 200, ref))

	if r.status != http.StatusUnprocessableEntity || problemCode(t, r) != "idempotency_key_reused" {
		t.Fatalf("another payload = %d %s, want 422 idempotency_key_reused", r.status, r.body)
	}
	if got := balanceOf(t, ts, cash); got != 100 {
		t.Errorf("balance = %d, want 100", got)
	}
}

// storedBusinessError checks that the POST with a new key answers status and code, and that the replay
// returns that same response. It returns the key.
func storedBusinessError(t *testing.T, ts *httptest.Server, body string, status int, code string) string {
	t.Helper()
	key := ids.New().String()
	first := postWithKey(t, ts, key, body)
	if first.status != status || problemCode(t, first) != code {
		t.Fatalf("= %d %s, want %d %s", first.status, first.body, status, code)
	}
	allTheSame(t, []response{postWithKey(t, ts, key, body)}, first)
	return key
}

// TestInsufficientFundsIsStored: a business 4xx is the response for that key, even if the same command could
// pass later.
func TestInsufficientFundsIsStored(t *testing.T) {
	ts, cash, deposit := newAccounts(t)
	body := postingBody(t, deposit, cash, 100, 100, "")
	key := storedBusinessError(t, ts, body, http.StatusUnprocessableEntity, "insufficient_funds")
	if r := postWithKey(t, ts, ids.New().String(), postingBody(t, cash, deposit, 1000, 1000, "")); r.status != http.StatusCreated {
		t.Fatalf("funding = %d %s", r.status, r.body)
	}

	if r := postWithKey(t, ts, key, body); r.status != http.StatusUnprocessableEntity {
		t.Errorf("replay with funds = %d %s, want the stored 422", r.status, r.body)
	}
}

// TestDuplicateExternalRefIsStored: a reference the entity already posted, with another key, is 409
// duplicate_external_ref, and that response is stored.
func TestDuplicateExternalRefIsStored(t *testing.T) {
	ts, cash, deposit := newAccounts(t)
	ref := ids.New().String()
	if r := postWithKey(t, ts, ids.New().String(), postingBody(t, cash, deposit, 1, 1, ref)); r.status != http.StatusCreated {
		t.Fatalf("posting with ref = %d %s", r.status, r.body)
	}

	storedBusinessError(t, ts, postingBody(t, cash, deposit, 1, 1, ref), http.StatusConflict, "duplicate_external_ref")
}

func TestAccountNotFoundIsStored(t *testing.T) {
	ts, cash, _ := newAccounts(t)

	storedBusinessError(t, ts, postingBody(t, cash, ids.New().String(), 1, 1, ""), http.StatusNotFound, "not_found")
}

// TestAnUnexpectedErrorIsNotStored: an error that is not a business one (a 500) goes up as is and is not
// stored, so the retry with the same key runs the command.
func TestAnUnexpectedErrorIsNotStored(t *testing.T) {
	cmds := commands{uow: postgres.NewUnitOfWork(testPool), keys: idempotency.NewStore(testPool)}
	op := operation{method: http.MethodPost, path: "/test", success: http.StatusCreated}
	key, down := ids.New().String(), errors.New("the database went down")
	failing := func(postgres.Tx) (any, error) { return nil, down }
	succeeding := func(postgres.Tx) (any, error) { return map[string]string{"ok": "yes"}, nil }

	ctx := withTestTrail(t)

	p := testPrincipal()
	if _, err := cmds.idempotent(ctx, p, op, key, "body", failing); !errors.Is(err, down) {
		t.Fatalf("= %v; the command error must go up as is", err)
	}
	resp, err := cmds.idempotent(ctx, p, op, key, "body", succeeding)

	if err != nil || resp.status != http.StatusCreated || string(resp.body) != "{\"ok\":\"yes\"}\n" {
		t.Fatalf("retry after a 500 = %+v, %v; want the command to run", resp, err)
	}
}

// TestARequestInFlightIs409: if another request with the same key does not finish within the pool's
// lock_timeout, the second gets 409 request_in_progress and does not post.
func TestARequestInFlightIs409(t *testing.T) {
	entity := ids.New()
	ts := apiServer(t, entity)
	cash, deposit := createAccount(t, ts, "asset"), createAccount(t, ts, "liability")
	key := ids.New().String()
	claimed, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce() // if the test fails earlier, the first transaction is not left open
	go func() {
		done <- postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
			scope := keyScope(access.Principal{Subject: testSubject, LegalEntityID: entity}, postTransaction)
			if _, err := idempotency.NewStore(testPool).Claim(t.Context(), tx, scope, key, [32]byte{}); err != nil {
				return err
			}
			close(claimed)
			<-release
			return errors.New("the first is rolled back")
		})
	}()
	select {
	case <-claimed:
	case err := <-done:
		t.Fatalf("the first did not claim the key: %v", err)
	}

	r := postWithKey(t, ts, key, postingBody(t, cash, deposit, 100, 100, ""))
	releaseOnce()
	if err := <-done; err == nil {
		t.Fatal("the first transaction should have rolled back")
	}

	if r.status != http.StatusConflict || problemCode(t, r) != "request_in_progress" {
		t.Fatalf("in flight = %d %s, want 409 request_in_progress", r.status, r.body)
	}
	if got := balanceOf(t, ts, cash); got != 0 {
		t.Errorf("balance = %d, want 0", got)
	}
}

// TestConcurrentRequestsPostOnce: 20 simultaneous requests with the same key post a single transaction, and
// all receive the same response: those that arrive while the first runs wait for it.
func TestConcurrentRequestsPostOnce(t *testing.T) {
	ts, cash, deposit := newAccounts(t)
	key, ref := ids.New().String(), ids.New().String()

	responses := concurrently(t, 20, ts, key, postingBody(t, cash, deposit, 100, 100, ref))

	if responses[0].status != http.StatusCreated {
		t.Fatalf("response = %d %s, want 201", responses[0].status, responses[0].body)
	}
	allTheSame(t, responses, responses[0])
	var count int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM ledger.transactions WHERE external_ref = $1`, ref).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("posted transactions = %d, want exactly 1", count)
	}
}

// TestConcurrentReplaysAllGetTheResponse: several simultaneous retries of an already completed key all read
// its response; none gets 409.
func TestConcurrentReplaysAllGetTheResponse(t *testing.T) {
	ts, cash, deposit := newAccounts(t)
	key, body := ids.New().String(), postingBody(t, cash, deposit, 100, 100, "")
	first := postWithKey(t, ts, key, body)

	allTheSame(t, concurrently(t, 30, ts, key, body), first)
}

// TestKeysAreScopedByEntity: two entities sharing the database can use the same key.
func TestKeysAreScopedByEntity(t *testing.T) {
	key := ids.New().String()
	for range 2 {
		ts, cash, deposit := newAccounts(t)
		if r := postWithKey(t, ts, key, postingBody(t, cash, deposit, 100, 100, "")); r.status != http.StatusCreated {
			t.Fatalf("the same key in another entity = %d %s, want 201", r.status, r.body)
		}
	}
}

// TestKeysAreScopedBySubject: two subjects of one entity do not share keys; the same key with the same body
// from another subject is not a retry and does not receive the first one's response.
func TestKeysAreScopedBySubject(t *testing.T) {
	cmds := commands{uow: postgres.NewUnitOfWork(testPool), keys: idempotency.NewStore(testPool)}
	op := operation{method: http.MethodPost, path: "/test", success: http.StatusCreated}
	key, entity := ids.New().String(), ids.New()
	runs := 0
	effect := func(postgres.Tx) (any, error) { runs++; return map[string]int{"run": runs}, nil }

	for _, subject := range []string{"subject-a", "subject-b"} {
		p := access.Principal{Subject: subject, LegalEntityID: entity}
		if _, err := cmds.idempotent(withTestTrail(t), p, op, key, "body", effect); err != nil {
			t.Fatal(err)
		}
	}

	if runs != 2 {
		t.Fatalf("the command ran %d times for two subjects with the same key, want 2", runs)
	}
}

// TestACommandWithoutTrailFailsClosed: without an audit trail in the context, a command does not run.
func TestACommandWithoutTrailFailsClosed(t *testing.T) {
	cmds := commands{uow: postgres.NewUnitOfWork(testPool), keys: idempotency.NewStore(testPool)}
	op := operation{method: http.MethodPost, path: "/test", success: http.StatusCreated}
	ran := false
	effect := func(postgres.Tx) (any, error) { ran = true; return map[string]string{}, nil }

	_, runErr := cmds.run(t.Context(), op, effect)
	_, keyErr := cmds.idempotent(t.Context(), testPrincipal(), op, ids.New().String(), "body", effect)

	if !errors.Is(runErr, errNoAuditTrail) || !errors.Is(keyErr, errNoAuditTrail) || ran {
		t.Fatalf("no trail: run = %v, idempotent = %v, effect ran: %v", runErr, keyErr, ran)
	}
}

// withTestTrail is a context with the audit trail that withAudit sets, to run a command without going
// through the server.
func withTestTrail(t *testing.T) context.Context {
	t.Helper()
	return context.WithValue(t.Context(), auditTrailKey{}, &auditTrail{records: audit.New(), base: audit.Record{
		Actor: "test", Role: "test", Channel: "test", CorrelationID: ids.New().String(), Command: "POST /test",
	}})
}

// testPrincipal is an operator of a new entity.
func testPrincipal() access.Principal {
	return access.Principal{Subject: testSubject, LegalEntityID: ids.New(), Roles: []string{testRole}}
}
