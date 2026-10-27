//go:build integration

package httpapi

import (
	"bytes"
	"encoding/json"
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

	"github.com/DrewGGM/aluna/internal/audit"
	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, &testPool)) }

// apiServer levanta la API completa (middleware, auditoría, código generado y handlers) de la entidad
// entity.
func apiServer(t *testing.T, entity ids.ID) *httptest.Server {
	t.Helper()
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	return apiServerWith(t, entity, events)
}

// apiServerWith es apiServer con los eventos del ledger en events.
func apiServerWith(t *testing.T, entity ids.ID, events ledger.Events) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	api := API{LedgerHandlers: NewLedgerHandlers(ledger.New(calendar.New(), events), postgres.NewUnitOfWork(testPool), idempotency.NewStore(testPool), testPool, entity)}
	cfg := ServerConfig{Audit: &CommandAudit{Records: audit.New(), UoW: postgres.NewUnitOfWork(testPool)}}
	ts := httptest.NewServer(NewServer(cfg, Routes(api, log), log).Handler)
	t.Cleanup(ts.Close)
	return ts
}

// call hace un request JSON, decodifica la respuesta en out (si no es nil) y devuelve el status.
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
	defer resp.Body.Close() //nolint:errcheck // cierre en prueba
	if _, isProblem := out.(*Problem); isProblem && resp.Header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("%s %s = %d con Content-Type %q, se esperaba application/problem+json", method, path, resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s = %d: cuerpo inválido: %v", method, path, resp.StatusCode, err)
		}
	}
	return resp.StatusCode
}

func createAccount(t *testing.T, ts *httptest.Server, kind string) string {
	t.Helper()
	var account gen.LedgerAccount
	status := call(t, ts, http.MethodPost, "/v1/ledger-accounts",
		`{"code":"cta-`+ids.New().String()+`","kind":"`+kind+`","currency":"COP","balance_mode":"sync"}`, &account)
	if status != http.StatusCreated {
		t.Fatalf("crear cuenta = %d", status)
	}
	return account.Id.String()
}

// postingValueDate es la fecha valor de los postings de postingBody.
const postingValueDate = "2026-10-26"

// postingBody arma el cuerpo de un posting en COP: debitUnits al débito y creditUnits (positivo) al
// crédito. ref repite la referencia externa si no es vacía.
func postingBody(t *testing.T, debit, credit string, debitUnits, creditUnits int64, ref string) string {
	t.Helper()
	if ref == "" {
		ref = ids.New().String()
	}
	b, err := json.Marshal(map[string]any{
		"code": "deposito", "value_date": postingValueDate, "business_date": postingValueDate, "source": "test",
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
		t.Fatalf("asentar = %d", status)
	}
	var read gen.Transaction
	if status := call(t, ts, http.MethodGet, "/v1/transactions/"+posted.Id.String(), "", &read); status != http.StatusOK || read.ExternalRef != posted.ExternalRef || len(read.Entries) != 2 {
		t.Fatalf("leer la transacción = %d %+v", status, read)
	}
	var balance gen.Balance
	call(t, ts, http.MethodGet, "/v1/ledger-accounts/"+deposit+"/balance", "", &balance)
	if balance.Posted.Amount != -10_000_000 || balance.Natural.Amount != 10_000_000 || balance.Natural.Currency != "COP" {
		t.Fatalf("saldo del depósito = %+v: con signo -10.000.000 y natural 10.000.000 COP", balance)
	}
	var account gen.LedgerAccount
	if status := call(t, ts, http.MethodGet, "/v1/ledger-accounts/"+cash, "", &account); status != http.StatusOK || account.Kind != "asset" {
		t.Fatalf("leer la cuenta = %d %+v", status, account)
	}
}

func TestLedgerErrorsAreProblems(t *testing.T) {
	ts := apiServer(t, ids.New())
	cash, deposit := createAccount(t, ts, "asset"), createAccount(t, ts, "liability")
	repeated := ids.New().String()
	if status := call(t, ts, http.MethodPost, "/v1/transactions", postingBody(t, cash, deposit, 100, 100, repeated), nil); status != http.StatusCreated {
		t.Fatalf("primer posting = %d", status)
	}
	cases := map[string]struct {
		method, path, body string
		status             int
		code               string
	}{
		"fondos insuficientes":    {http.MethodPost, "/v1/transactions", postingBody(t, deposit, cash, 101, 101, ""), 422, "insufficient_funds"},
		"desbalanceada":           {http.MethodPost, "/v1/transactions", postingBody(t, cash, deposit, 100, 99, ""), 422, "unbalanced_transaction"},
		"referencia repetida":     {http.MethodPost, "/v1/transactions", postingBody(t, cash, deposit, 100, 100, repeated), 409, "duplicate_external_ref"},
		"moneda desconocida":      {http.MethodPost, "/v1/ledger-accounts", `{"code":"x","kind":"asset","currency":"XYZ","balance_mode":"sync"}`, 422, "unknown_currency"},
		"cuenta inexistente":      {http.MethodGet, "/v1/ledger-accounts/" + ids.New().String(), "", 404, "not_found"},
		"transacción inexistente": {http.MethodGet, "/v1/transactions/" + ids.New().String(), "", 404, "not_found"},
		"id que no es uuid":       {http.MethodGet, "/v1/ledger-accounts/123", "", 400, "invalid_request"},
		"JSON roto":               {http.MethodPost, "/v1/transactions", `{"code":`, 400, "invalid_request"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var p Problem
			status := call(t, ts, tc.method, tc.path, tc.body, &p)

			if status != tc.status || p.Code != tc.code {
				t.Fatalf("= %d %+v, se esperaba %d %s", status, p, tc.status, tc.code)
			}
			if strings.Contains(p.Detail, "pgx") || strings.Contains(p.Detail, "ledger.") || strings.Contains(p.Detail, "SQLSTATE") {
				t.Fatalf("el problem filtra detalles internos: %q", p.Detail)
			}
		})
	}
}

// TestOtherEntitiesResourcesAreNotVisible: el despliegue de una entidad no ve cuentas ni transacciones
// de otra aunque compartan la base.
func TestOtherEntitiesResourcesAreNotVisible(t *testing.T) {
	mine, theirs := apiServer(t, ids.New()), apiServer(t, ids.New())
	cash, deposit := createAccount(t, theirs, "asset"), createAccount(t, theirs, "liability")
	var posted gen.Transaction
	if status := call(t, theirs, http.MethodPost, "/v1/transactions", postingBody(t, cash, deposit, 100, 100, ""), &posted); status != http.StatusCreated {
		t.Fatalf("el posting de la otra entidad = %d", status)
	}

	for _, path := range []string{"/v1/ledger-accounts/" + cash, "/v1/ledger-accounts/" + cash + "/balance", "/v1/transactions/" + posted.Id.String()} {
		var p Problem
		if status := call(t, mine, http.MethodGet, path, "", &p); status != http.StatusNotFound || p.Code != "not_found" {
			t.Errorf("GET %s desde otra entidad = %d %+v, se esperaba 404", path, status, p)
		}
	}
	if status := call(t, mine, http.MethodPost, "/v1/transactions", postingBody(t, cash, deposit, 1, 1, ""), nil); status != http.StatusNotFound {
		t.Errorf("asentar sobre cuentas de otra entidad = %d, se esperaba 404", status)
	}
}

// TestBigBodyWithoutLengthIs413: un cuerpo sin Content-Length lo corta withBodyLimit y el código
// generado lo responde como 413.
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
	defer resp.Body.Close() //nolint:errcheck // cierre en prueba

	var p Problem
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil || resp.StatusCode != http.StatusRequestEntityTooLarge || p.Code != "body_too_large" {
		t.Fatalf("= %d %+v (%v), se esperaba 413 body_too_large", resp.StatusCode, p, err)
	}
}

// TestTheBitemporalBalanceOverHTTP (FR-8): con cortes, el saldo se suma de los entries y trae los
// cortes con que se calculó, sin pendientes; el posting no está a la fecha valor del día anterior ni como
// se sabía hace una hora.
func TestTheBitemporalBalanceOverHTTP(t *testing.T) {
	ts := apiServer(t, ids.New())
	cash, deposit := createAccount(t, ts, "asset"), createAccount(t, ts, "liability")
	if status := call(t, ts, http.MethodPost, "/v1/transactions", postingBody(t, cash, deposit, 500, 500, ""), nil); status != http.StatusCreated {
		t.Fatalf("asentar = %d", status)
	}
	anHourAgo := url.QueryEscape(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339))
	cases := map[string]struct {
		query   string
		natural int64
	}{
		"a su fecha valor":       {"as_of_value_date=" + postingValueDate, 500},
		"el día anterior":        {"as_of_value_date=" + dayBefore(t, postingValueDate), 0},
		"como se sabía hace 1 h": {"as_known_at=" + anHourAgo, 0},
	}
	for name, tc := range cases {
		b := balanceAt(t, ts, deposit, "?"+tc.query)
		if b.Natural.Amount != tc.natural || echoedCut(b) != tc.query || b.PendingDebits != nil || b.PendingCredits != nil || b.Version != nil {
			t.Errorf("%s = %+v, se esperaba natural %d con su corte y sin pendientes", name, b, tc.natural)
		}
	}
}

// TestWithoutCutsTheBalanceIsTheMaterialized: sin cortes, o con los parámetros vacíos como los deja una
// plantilla de URL, el saldo es el materializado, con su versión.
func TestWithoutCutsTheBalanceIsTheMaterialized(t *testing.T) {
	ts := apiServer(t, ids.New())
	cash, deposit := createAccount(t, ts, "asset"), createAccount(t, ts, "liability")
	if status := call(t, ts, http.MethodPost, "/v1/transactions", postingBody(t, cash, deposit, 500, 500, ""), nil); status != http.StatusCreated {
		t.Fatalf("asentar = %d", status)
	}
	for _, query := range []string{"", "?as_of_value_date=&as_known_at="} {
		if b := balanceAt(t, ts, deposit, query); b.Natural.Amount != 500 || b.Version == nil || b.AsKnownAt != nil || b.AsOfValueDate != nil {
			t.Errorf("saldo%s = %+v, se esperaba el materializado con su versión", query, b)
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

// echoedCut es el corte que devolvió el saldo bitemporal, escrito como el parámetro que lo pidió.
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
		t.Fatalf("saldo%s = %d", query, status)
	}
	return b
}

// TestAClosedPeriodIsAConflict (FR-15): asentar con una fecha de negocio en un periodo cerrado es 409
// period_closed.
func TestAClosedPeriodIsAConflict(t *testing.T) {
	entity := ids.New()
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error { // las entidades y los cierres no son de la aplicación
		if _, err := tx.Exec(t.Context(), `INSERT INTO calendar.legal_entities (id, name, business_date, functional_currency)
			VALUES ($1, 'Entidad', '2025-07-07', 'COP')`, entity); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), `INSERT INTO calendar.periods (legal_entity_id, period, status, closed_at, closed_by)
			VALUES ($1, '2025-06', 'closed', clock_timestamp(), 'contador')`, entity)
		return err
	})
	ts := apiServer(t, entity)
	cash, deposit := createAccount(t, ts, "asset"), createAccount(t, ts, "liability")
	body := strings.Replace(postingBody(t, cash, deposit, 100, 100, ""), `"business_date":"`+postingValueDate, `"business_date":"2025-06-30`, 1)

	r := postWithKey(t, ts, ids.New().String(), body)

	if r.status != http.StatusConflict || problemCode(t, r) != "period_closed" {
		t.Fatalf("posting en junio = %d %s, se esperaba 409 period_closed", r.status, r.body)
	}
}

// TestTheZeroBusinessDateIsInvalid: por la API la fecha de negocio es de quien llama; 0001-01-01 no se
// toma como ausente (el ledger pondría la de la entidad) sino como inválida.
func TestTheZeroBusinessDateIsInvalid(t *testing.T) {
	ts := apiServer(t, ids.New())
	cash, deposit := createAccount(t, ts, "asset"), createAccount(t, ts, "liability")
	body := strings.Replace(postingBody(t, cash, deposit, 100, 100, ""), `"business_date":"`+postingValueDate, `"business_date":"0001-01-01`, 1)

	r := postWithKey(t, ts, ids.New().String(), body)

	if r.status != http.StatusUnprocessableEntity || problemCode(t, r) != "invalid_posting" {
		t.Fatalf("posting con fecha de negocio cero = %d %s, se esperaba 422 invalid_posting", r.status, r.body)
	}
}
