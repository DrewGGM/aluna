//go:build integration

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/accounting"
	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// cuifWorld is a new entity with two finance subjects: the maker and the checker.
type cuifWorld struct {
	entity ids.ID
	a, b   *httptest.Server
}

func newCuifWorld(t *testing.T) cuifWorld {
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
	return cuifWorld{
		entity: entity, a: apiServerAsRole(t, entity, events, "finance-a", "finance"), b: apiServerAsRole(t, entity, events, "finance-b", "finance"),
	}
}

func cuifBody(rows string) string {
	body, err := json.Marshal(map[string]string{"csv": "pattern,cuif_code,valid_from,valid_to\n" + rows})
	if err != nil {
		panic(err)
	}
	return string(body)
}

func (w cuifWorld) mapped(t *testing.T, account string) (string, error) {
	t.Helper()
	return accounting.New().MapToCUIF(t.Context(), testPool, w.entity, account, calendar.Date{Year: 2026, Month: time.March, Day: 2})
}

// TestAFileOfMappingsLoadsWithMakerChecker (FR-36, FR-39): nothing holds until someone else approves it, and the maker
// cannot be the checker.
func TestAFileOfMappingsLoadsWithMakerChecker(t *testing.T) {
	w := newCuifWorld(t)

	requested := postTo(t, w.a, "/v1/cuif-mappings", ids.New().String(), cuifBody("L.TAX.GMF,244500,2026-03-02,2028-01-01\n"))
	if requested.status != http.StatusAccepted {
		t.Fatalf("request = %d %s", requested.status, requested.body)
	}
	if _, err := w.mapped(t, "L.TAX.GMF"); err == nil {
		t.Fatal("it holds before the approval")
	}

	path := "/v1/approvals/" + decodeApproval(t, requested).ID + "/approval"
	if own := postTo(t, w.a, path, ids.New().String(), ""); own.status != http.StatusForbidden {
		t.Fatalf("the maker approving = %d %s", own.status, own.body)
	}
	approved := postTo(t, w.b, path, ids.New().String(), "")
	if approved.status != http.StatusOK || !strings.Contains(string(approved.body), `"mappings":1`) {
		t.Fatalf("approval = %d %s, want one mapping stored", approved.status, approved.body)
	}
	if got, err := w.mapped(t, "L.TAX.GMF"); err != nil || got != "244500" {
		t.Fatalf("mapped to %s, %v", got, err)
	}
}

// TestAFileIsCheckedWhenRequestedAndWhenApproved: two requests can wait at once, and the second to be approved fails.
func TestAFileIsCheckedWhenRequestedAndWhenApproved(t *testing.T) {
	w := newCuifWorld(t)
	file := cuifBody("L.,210000,2026-03-02,2027-01-01\n")
	first, second := decodeApproval(t, postTo(t, w.a, "/v1/cuif-mappings", ids.New().String(), file)), decodeApproval(t, postTo(t, w.a, "/v1/cuif-mappings", ids.New().String(), file))

	if r := postTo(t, w.b, "/v1/approvals/"+first.ID+"/approval", ids.New().String(), ""); r.status != http.StatusOK {
		t.Fatalf("first approval = %d %s", r.status, r.body)
	}
	failed := postTo(t, w.b, "/v1/approvals/"+second.ID+"/approval", ids.New().String(), "")
	if failed.status != http.StatusOK || decodeApproval(t, failed).Status != "failed" {
		t.Fatalf("second approval = %d %s, want the request failed", failed.status, failed.body)
	}

	expectProblem(t, "the same file again", postTo(t, w.a, "/v1/cuif-mappings", ids.New().String(), file), http.StatusConflict, "mapping_overlap")
	key := ids.New().String()
	bad := postTo(t, w.a, "/v1/cuif-mappings", key, cuifBody("L.,1,2026-03-02,2027-01-01\n"))
	expectProblem(t, "a bad file", bad, http.StatusUnprocessableEntity, "invalid_mapping")
	if !strings.Contains(string(bad.body), "line 2") {
		t.Errorf("a bad file = %s, want its line", bad.body)
	}
	expectProblem(t, "a file that starts in the past", postTo(t, w.a, "/v1/cuif-mappings", ids.New().String(), cuifBody("L.,210000,2026-03-01,2027-01-01\n")),
		http.StatusUnprocessableEntity, "invalid_mapping")
	expectProblem(t, "the bad file again, by its key", postTo(t, w.a, "/v1/cuif-mappings", key, cuifBody("L.,1,2026-03-02,2027-01-01\n")),
		http.StatusUnprocessableEntity, "invalid_mapping")
	expectProblem(t, "another file with the key", postTo(t, w.a, "/v1/cuif-mappings", key, cuifBody("L.,210000,2030-01-01,2031-01-01\n")),
		http.StatusUnprocessableEntity, "idempotency_key_reused")
	expectProblem(t, "overlapping rows", postTo(t, w.a, "/v1/cuif-mappings", ids.New().String(),
		cuifBody("A.,110000,2026-03-02,2027-01-01\nA.,110001,2026-06-01,2027-01-01\n")), http.StatusConflict, "mapping_overlap")
}

// TestTheGLOfADayOverHTTP (FR-36, US-13): a projected day answers its lines, with debits and credits apart, and one
// without a projection is 404.
func TestTheGLOfADayOverHTTP(t *testing.T) {
	w := newCuifWorld(t)
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `INSERT INTO accounting.gl_days (legal_entity_id, business_date, status, seen_below)
			VALUES ($1, '2026-03-01', 'projected', '1'), ($1, '2026-02-28', 'unmapped', '1')`, w.entity); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), `INSERT INTO accounting.gl_lines (legal_entity_id, business_date, cuif_code, currency, debit_units, credit_units)
			VALUES ($1, '2026-03-01', '110505', 'COP', 100, 0), ($1, '2026-03-01', '210505', 'COP', 0, 100)`, w.entity)
		return err
	})

	var gl gen.DailyGL
	if status := call(t, w.a, http.MethodGet, "/v1/general-ledger/daily?date=2026-03-01", "", &gl); status != http.StatusOK {
		t.Fatalf("GL = %d", status)
	}
	want := []gen.GLLine{
		{CuifCode: "110505", Debits: gen.Money{Amount: 100, Currency: "COP"}, Credits: gen.Money{Amount: 0, Currency: "COP"}},
		{CuifCode: "210505", Debits: gen.Money{Amount: 0, Currency: "COP"}, Credits: gen.Money{Amount: 100, Currency: "COP"}},
	}
	if !slices.Equal(gl.Lines, want) || gl.Date.String() != "2026-03-01" || gl.ProjectedAt.IsZero() {
		t.Fatalf("GL = %+v, want %+v", gl, want)
	}
	var p Problem
	if status := call(t, w.a, http.MethodGet, "/v1/general-ledger/daily?date=2026-03-02", "", &p); status != http.StatusNotFound || p.Code != "gl_day_not_projected" {
		t.Fatalf("a day without a projection = %d %+v", status, p)
	}
	if status := call(t, w.a, http.MethodGet, "/v1/general-ledger/daily?date=2026-02-28", "", &p); status != http.StatusConflict || p.Code != "gl_day_unmapped" {
		t.Fatalf("an unmapped day = %d %+v", status, p)
	}
}

// TestTheAuditorReadsTheGL: reading it is not loading mappings.
func TestTheAuditorReadsTheGL(t *testing.T) {
	w := newCuifWorld(t)
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	auditor := apiServerAsRole(t, w.entity, events, "auditor-a", "auditor")
	var p Problem
	if status := call(t, auditor, http.MethodGet, "/v1/general-ledger/daily?date=2026-03-01", "", &p); status != http.StatusNotFound || p.Code != "gl_day_not_projected" {
		t.Fatalf("the auditor reads = %d %+v, want the 404 of a day without a projection", status, p)
	}
}

// TestOnlyFinanceLoadsMappings: an operator does not.
func TestOnlyFinanceLoadsMappings(t *testing.T) {
	entity := ids.New()
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	operator := apiServerAsRole(t, entity, events, "operator-a", "operator")
	if r := postTo(t, operator, "/v1/cuif-mappings", ids.New().String(), cuifBody("L.,210000,2026-03-02,2027-01-01\n")); r.status != http.StatusForbidden {
		t.Fatalf("operator = %d %s, want 403", r.status, r.body)
	}
}
