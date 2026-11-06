//go:build integration

package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// reconServer is the API as a finance subject of a new entity.
func reconServer(t *testing.T, role string) *httptest.Server {
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
	return apiServerAsRole(t, entity, events, "finance-a", role)
}

func statementBody(ref string, units int64) string {
	return fmt.Sprintf(`{"rail":"simbreb","statement_ref":%q,"statement_date":"2026-03-02","lines":[
		{"end_to_end_id":"E2E","direction":"outgoing","amount":{"amount":%d,"currency":"COP"},"booking_date":"2026-03-02"}]}`, ref, units)
}

// listed is the page of breaks in status.
func listed(t *testing.T, ts *httptest.Server, status string) []gen.Break {
	t.Helper()
	var list gen.BreakList
	if code := call(t, ts, http.MethodGet, "/v1/reconciliation/breaks?status="+status, "", &list); code != http.StatusOK {
		t.Fatalf("list %s = %d", status, code)
	}
	return list.Breaks
}

// expectOpenBreak checks the only break listed as open, of a line nobody knows, and returns it.
func expectOpenBreak(t *testing.T, ts *httptest.Server) gen.Break {
	t.Helper()
	breaks := listed(t, ts, "open")
	if len(breaks) != 1 {
		t.Fatalf("open = %+v, want one", breaks)
	}
	b := breaks[0]
	got := [...]string{b.Subject, b.Owner, string(b.Status), deref(b.Reason), deref(b.Cause)}
	if want := [...]string{"outgoing/E2E", "finance-a", "open", "line_without_order", ""}; got != want || b.Difference.Amount != 100 || b.ResolvedAt != nil {
		t.Fatalf("break = %+v, want %v", b, want)
	}
	return b
}

// expectResolved checks the answer to a resolution, and that the break moved to the resolved ones.
func expectResolved(t *testing.T, ts *httptest.Server, resolved response) {
	t.Helper()
	if resolved.status != http.StatusOK {
		t.Fatalf("resolve = %d %s", resolved.status, resolved.body)
	}
	var read gen.Break
	if err := json.Unmarshal(resolved.body, &read); err != nil {
		t.Fatal(err)
	}
	if got, want := [...]string{string(read.Status), deref(read.Cause), deref(read.ResolvedBy)}, [...]string{"resolved", "a line of a test", "finance-a"}; got != want || read.ResolvedAt == nil {
		t.Fatalf("resolved = %+v, want %v and when", read, want)
	}
	if open := listed(t, ts, "open"); len(open) != 0 {
		t.Fatalf("open = %+v, want none", open)
	}
	if closed := listed(t, ts, "resolved"); len(closed) != 1 || closed[0].ResolvedAt == nil {
		t.Fatalf("resolved list = %+v", closed)
	}
}

// TestAStatementOverHTTP (FR-34, US-9): an import with a line nobody knows answers with its count and leaves a
// break to list, which is resolved with a cause; a retry of the key answers the same, without a second break.
func TestAStatementOverHTTP(t *testing.T) {
	ts := reconServer(t, "finance")
	key := ids.New().String()

	imported, retried := postTo(t, ts, "/v1/reconciliation/statements", key, statementBody("STMT-1", 100)),
		postTo(t, ts, "/v1/reconciliation/statements", key, statementBody("STMT-1", 100))
	if imported.status != http.StatusCreated || !bytes.Equal(imported.body, retried.body) {
		t.Fatalf("import = %d %s, retry %d %s", imported.status, imported.body, retried.status, retried.body)
	}
	var result gen.StatementImport
	if err := json.Unmarshal(imported.body, &result); err != nil || result != (gen.StatementImport{Id: result.Id, Lines: 1, Matched: 0, Exceptions: 1}) {
		t.Fatalf("result = %+v (%v)", result, err)
	}

	found := expectOpenBreak(t, ts)
	if found.StatementId == nil || *found.StatementId != result.Id {
		t.Fatalf("break of statement %v, want %v", found.StatementId, result.Id)
	}
	path := "/v1/reconciliation/breaks/" + found.Id.String() + "/resolution"
	expectResolved(t, ts, postTo(t, ts, path, ids.New().String(), `{"cause":"a line of a test"}`))
}

func TestReconciliationErrorsAreProblems(t *testing.T) {
	ts := reconServer(t, "finance")
	statements := "/v1/reconciliation/statements"
	if r := postTo(t, ts, statements, ids.New().String(), statementBody("STMT-1", 100)); r.status != http.StatusCreated {
		t.Fatalf("import = %d %s", r.status, r.body)
	}
	var list gen.BreakList
	if status := call(t, ts, http.MethodGet, "/v1/reconciliation/breaks", "", &list); status != http.StatusOK || len(list.Breaks) != 1 {
		t.Fatalf("list = %d %+v", status, list)
	}
	resolution := "/v1/reconciliation/breaks/" + list.Breaks[0].Id.String() + "/resolution"
	if r := postTo(t, ts, resolution, ids.New().String(), `{"cause":"first"}`); r.status != http.StatusOK {
		t.Fatalf("resolve = %d %s", r.status, r.body)
	}

	for name, c := range map[string]struct {
		path, body string
		status     int
		code       string
	}{
		"the same statement": {statements, statementBody("STMT-1", 100), http.StatusConflict, "duplicate_statement"},
		"a zero amount":      {statements, statementBody("STMT-2", 0), http.StatusUnprocessableEntity, "invalid_statement"},
		"an unknown break":   {"/v1/reconciliation/breaks/" + ids.New().String() + "/resolution", `{"cause":"c"}`, http.StatusNotFound, "not_found"},
		"resolved twice":     {resolution, `{"cause":"again"}`, http.StatusConflict, "break_already_resolved"},
		"no cause":           {resolution, `{"cause":""}`, http.StatusUnprocessableEntity, "invalid_resolution"},
		"no lines":           {statements, `{"rail":"simbreb","statement_ref":"STMT-3","statement_date":"2026-03-02"}`, http.StatusUnprocessableEntity, "invalid_statement"},
		"an unknown rail":    {statements, `{"rail":"swift","statement_ref":"STMT-4","statement_date":"2026-03-02","lines":[]}`, http.StatusUnprocessableEntity, "invalid_statement"},
	} {
		expectProblem(t, name, postTo(t, ts, c.path, ids.New().String(), c.body), c.status, c.code)
	}
	for _, query := range []string{"cursor=nope", "limit=0", "limit=-1", "limit=201", "status=bogus"} {
		var p Problem
		if status := call(t, ts, http.MethodGet, "/v1/reconciliation/breaks?"+query, "", &p); status != http.StatusBadRequest {
			t.Errorf("%s = %d %+v, want 400", query, status, p)
		}
	}
}

// TestADayWithoutMovementsIsAStatement: an empty list of lines is one, and no list is not.
func TestADayWithoutMovementsIsAStatement(t *testing.T) {
	ts := reconServer(t, "finance")
	r := postTo(t, ts, "/v1/reconciliation/statements", ids.New().String(), `{"rail":"simbreb","statement_ref":"STMT-1","statement_date":"2026-03-02","lines":[]}`)
	if r.status != http.StatusCreated {
		t.Fatalf("import = %d %s, want 201", r.status, r.body)
	}
}

// TestOnlyFinanceWorksTheBreaks: an operator gets nothing; an auditor reads them and resolves none.
func TestOnlyFinanceWorksTheBreaks(t *testing.T) {
	operator, auditor := reconServer(t, "operator"), reconServer(t, "auditor")
	if status := call(t, operator, http.MethodGet, "/v1/reconciliation/breaks", "", nil); status != http.StatusForbidden {
		t.Errorf("operator reads = %d, want 403", status)
	}
	if r := postTo(t, operator, "/v1/reconciliation/statements", ids.New().String(), statementBody("STMT-1", 100)); r.status != http.StatusForbidden {
		t.Errorf("operator imports = %d, want 403", r.status)
	}
	if status := call(t, auditor, http.MethodGet, "/v1/reconciliation/breaks", "", nil); status != http.StatusOK {
		t.Errorf("auditor reads = %d, want 200", status)
	}
	if r := postTo(t, auditor, "/v1/reconciliation/breaks/"+ids.New().String()+"/resolution", ids.New().String(), `{"cause":"c"}`); r.status != http.StatusForbidden {
		t.Errorf("auditor resolves = %d, want 403", r.status)
	}
}
