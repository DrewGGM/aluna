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

// auditedRow es un registro de auditoría con el xmin de su fila: la transacción que lo escribió.
type auditedRow struct {
	audit.Record
	xmin string
}

// auditedRequest hace un request con un X-Request-Id propio y devuelve la respuesta y su correlación.
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
	defer resp.Body.Close() //nolint:errcheck // cierre en prueba
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{status: resp.StatusCode, body: b}, correlation
}

// auditOf son, como dueño (aluna_app solo inserta), los registros de la correlación en orden. after se lee
// con números exactos.
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

// textOf es, como dueño, el único valor de query como texto.
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
		t.Fatalf("%d registros de auditoría para %s, se esperaba 1", len(rows), correlation)
	}
	return rows[0]
}

// codeOf es el código del problem de r.
func codeOf(t *testing.T, r response) string {
	t.Helper()
	var p Problem
	if err := json.Unmarshal(r.body, &p); err != nil || p.Code == "" {
		t.Fatalf("respuesta %d sin problem: %s", r.status, r.body)
	}
	return p.Code
}

// TestACommandIsAuditedInItsTransaction (FR-40): un POST exitoso deja su registro en la misma transacción
// que su efecto: quién, desde dónde, el comando, los IDs que tocó (la transacción y sus cuentas) y un
// resumen sin el texto libre del cliente, con los montos exactos aunque pasen de 2^53.
func TestACommandIsAuditedInItsTransaction(t *testing.T) {
	ts, cash, deposit := newAccounts(t)
	const big = 9007199254740993 // 2^53 + 1: un float64 no lo representa

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
		t.Fatalf("registro = %+v, se esperaba %+v", got.Record, want)
	}
	checkPostingSummary(t, got.After, body)
	if tx := textOf(t, `SELECT xmin::text FROM ledger.transactions WHERE id = $1`, id); got.xmin != tx {
		t.Fatalf("el registro lo escribió la transacción %s y el posting la %s", got.xmin, tx)
	}
}

// checkPostingSummary revisa que el after de un posting tenga su id y sus entries exactos, y nada del
// texto libre del cliente.
func checkPostingSummary(t *testing.T, after, body map[string]any) {
	t.Helper()
	for _, key := range []string{"code", "source", "external_ref"} {
		if _, ok := after[key]; ok {
			t.Errorf("after trae %q, texto libre del cliente", key)
		}
	}
	if after["id"] != body["id"] || !reflect.DeepEqual(after["entries"], body["entries"]) {
		t.Errorf("after = %v, se esperaban el id y los entries de la respuesta %v", after, body)
	}
}

// TestABusinessRejectionIsAuditedInItsTransaction: un rechazo de negocio dentro del comando deja su
// registro con el código del problem, en la transacción que guarda la respuesta de la Idempotency-Key;
// un reintento con la misma clave queda como replay.
func TestABusinessRejectionIsAuditedInItsTransaction(t *testing.T) {
	ts, cash, deposit := newAccounts(t)
	key := ids.New().String()
	overdraft := postingBody(t, deposit, cash, 100, 100, "") // el depósito vacío no puede pagar

	resp, correlation := auditedRequest(t, ts, http.MethodPost, "/v1/transactions", key, overdraft)
	_, replayed := auditedRequest(t, ts, http.MethodPost, "/v1/transactions", key, overdraft)

	got := onlyRecord(t, correlation)
	if code := codeOf(t, resp); got.Result != code || got.After != nil || len(got.Targets) != 0 {
		t.Fatalf("registro %+v, se esperaba el rechazo %s sin after ni targets", got.Record, code)
	}
	if stored := textOf(t, `SELECT xmin::text FROM infra.idempotency_keys WHERE key = $1`, key); got.xmin != stored {
		t.Fatalf("el registro lo escribió la transacción %s y la respuesta guardada la %s", got.xmin, stored)
	}
	if r := onlyRecord(t, replayed); r.Result != resultReplay {
		t.Fatalf("el reintento quedó como %q, se esperaba %q", r.Result, resultReplay)
	}
}

// TestARejectionBeforeTheCommandIsAudited: lo que se rechaza antes de la transacción del comando (una
// moneda desconocida, una Idempotency-Key reusada con otro cuerpo) también queda, con su código, en una
// transacción aparte.
func TestARejectionBeforeTheCommandIsAudited(t *testing.T) {
	ts, cash, deposit := newAccounts(t)
	key := ids.New().String()
	if first, _ := auditedRequest(t, ts, http.MethodPost, "/v1/transactions", key, postingBody(t, cash, deposit, 100, 100, "")); first.status != http.StatusCreated {
		t.Fatalf("primer posting = %d %s", first.status, first.body)
	}
	cases := map[string]func() (response, string){
		"moneda desconocida": func() (response, string) {
			return auditedRequest(t, ts, http.MethodPost, "/v1/ledger-accounts", "", `{"code":"cta-x","kind":"asset","currency":"XXX","balance_mode":"sync"}`)
		},
		"clave reusada": func() (response, string) {
			return auditedRequest(t, ts, http.MethodPost, "/v1/transactions", key, postingBody(t, cash, deposit, 200, 200, ""))
		},
	}
	for name, send := range cases {
		t.Run(name, func(t *testing.T) {
			resp, correlation := send()

			if got := onlyRecord(t, correlation); resp.status/100 != 4 || got.Result != codeOf(t, resp) {
				t.Fatalf("respuesta %d %s; registrado como %q", resp.status, resp.body, got.Result)
			}
		})
	}
}

type failingEvents struct{}

var errEventsDown = errors.New("el outbox no responde")

func (failingEvents) Emit(context.Context, postgres.Tx, outbox.Event) error { return errEventsDown }

// TestAFailedCommandIsAuditedApart: un comando que termina en 500 revierte su transacción, con su efecto
// y su respuesta; el registro queda igual, con el código del problem, en una transacción aparte.
func TestAFailedCommandIsAuditedApart(t *testing.T) {
	ts := apiServerWith(t, ids.New(), failingEvents{})
	cash, deposit := createAccount(t, ts, "asset"), createAccount(t, ts, "liability")
	key := ids.New().String()

	resp, correlation := auditedRequest(t, ts, http.MethodPost, "/v1/transactions", key, postingBody(t, cash, deposit, 100, 100, ""))

	got := onlyRecord(t, correlation)
	if resp.status != http.StatusInternalServerError || got.Result != codeOf(t, resp) || got.Command != "POST /v1/transactions" {
		t.Fatalf("respuesta %d; registro %+v, se esperaba un 500 registrado con su código", resp.status, got.Record)
	}
	if stored := textOf(t, `SELECT count(*)::text FROM infra.idempotency_keys WHERE key = $1`, key); stored != "0" {
		t.Fatalf("la respuesta del 500 quedó guardada (%s filas)", stored)
	}
}

// TestAPanickingCommandIsAudited: la auditoría va por fuera de la recuperación: un comando que hace panic
// queda registrado con el 500 que responde withRecovery.
func TestAPanickingCommandIsAudited(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := ServerConfig{Audit: &CommandAudit{Records: audit.New(), UoW: postgres.NewUnitOfWork(testPool)}}
	routes := func(mux *http.ServeMux) {
		mux.HandleFunc("POST /v1/panico", func(http.ResponseWriter, *http.Request) { panic("falla a propósito") })
	}
	ts := httptest.NewServer(NewServer(cfg, routes, log).Handler)
	t.Cleanup(ts.Close)

	resp, correlation := auditedRequest(t, ts, http.MethodPost, "/v1/panico", "", "{}")

	if got := onlyRecord(t, correlation); resp.status != http.StatusInternalServerError || got.Result != codeOf(t, resp) || got.Command != "POST /v1/panico" {
		t.Fatalf("respuesta %d; registro %+v", resp.status, got.Record)
	}
}

// TestCreatingAnAccountIsAudited: crear una cuenta, aunque no lleva Idempotency-Key, deja su registro en
// su transacción; repetir el código es un rechazo de negocio que también queda, con su código.
func TestCreatingAnAccountIsAudited(t *testing.T) {
	ts := apiServer(t, ids.New())
	body := `{"code":"cta-` + ids.New().String() + `","kind":"asset","currency":"COP","balance_mode":"sync"}`

	created, first := auditedRequest(t, ts, http.MethodPost, "/v1/ledger-accounts", "", body)
	repeated, second := auditedRequest(t, ts, http.MethodPost, "/v1/ledger-accounts", "", body)

	got := onlyRecord(t, first)
	id, _ := decodeExact(t, created.body)["id"].(string)
	if created.status != http.StatusCreated || got.Result != resultOK || !reflect.DeepEqual(got.Targets, []string{id}) {
		t.Fatalf("respuesta %d; registro %+v", created.status, got.Record)
	}
	if account := textOf(t, `SELECT xmin::text FROM ledger.accounts WHERE id = $1`, id); got.xmin != account {
		t.Fatalf("el registro lo escribió la transacción %s y la cuenta la %s", got.xmin, account)
	}
	if r := onlyRecord(t, second); r.Result != codeOf(t, repeated) {
		t.Fatalf("código repetido = %d, registrado como %q", repeated.status, r.Result)
	}
}

// TestACaptureIsAuditedWithTheHoldAndItsTransaction: el comando es la ruta con su parámetro y los targets
// incluyen el hold y la transacción que lo asentó.
func TestACaptureIsAuditedWithTheHoldAndItsTransaction(t *testing.T) {
	ts, _, hold := depositWithHold(t)

	resp, correlation := auditedRequest(t, ts, http.MethodPost, "/v1/holds/"+hold.Id.String()+"/capture", ids.New().String(), captureBody(200))

	got := onlyRecord(t, correlation)
	posted, _ := decodeExact(t, resp.body)["transaction"].(map[string]any)
	txID, _ := posted["id"].(string)
	if resp.status != http.StatusOK || got.Command != "POST /v1/holds/{id}/capture" || txID == "" ||
		!slices.Contains(got.Targets, hold.Id.String()) || !slices.Contains(got.Targets, txID) {
		t.Fatalf("respuesta %d; registro %s con targets %v, se esperaban el hold y %s", resp.status, got.Command, got.Targets, txID)
	}
}

// TestReadsAreNotAudited: un GET no es un comando y no deja registro.
func TestReadsAreNotAudited(t *testing.T) {
	ts, cash, _ := newAccounts(t)

	resp, correlation := auditedRequest(t, ts, http.MethodGet, "/v1/ledger-accounts/"+cash, "", "")

	if rows := auditOf(t, correlation); resp.status != http.StatusOK || len(rows) != 0 {
		t.Fatalf("GET = %d y dejó %d registros de auditoría", resp.status, len(rows))
	}
}

// TestAnUncleanPathIsAuditedOnce: el mux redirige /v1//transactions a la ruta limpia; el comando queda
// una sola vez, en el request que lo ejecuta, y la redirección no deja un registro de error.
func TestAnUncleanPathIsAuditedOnce(t *testing.T) {
	ts, cash, deposit := newAccounts(t)

	resp, correlation := auditedRequest(t, ts, http.MethodPost, "/v1//transactions", ids.New().String(), postingBody(t, cash, deposit, 100, 100, ""))

	if got := onlyRecord(t, correlation); resp.status != http.StatusCreated || got.Result != resultOK {
		t.Fatalf("respuesta %d; registro %+v", resp.status, got.Record)
	}
}

// TestARejectedHoldCommandNamesTheHold: un comando sobre un hold que se rechaza deja como target el hold
// de su ruta, dentro de la tx (un segundo release) o antes (una moneda desconocida, una clave inválida).
func TestARejectedHoldCommandNamesTheHold(t *testing.T) {
	ts, _, hold := depositWithHold(t)
	base := "/v1/holds/" + hold.Id.String()
	if first, _ := auditedRequest(t, ts, http.MethodPost, base+"/release", ids.New().String(), ""); first.status != http.StatusOK {
		t.Fatalf("primer release = %d %s", first.status, first.body)
	}
	cases := map[string]func() (response, string){
		"release repetido": func() (response, string) {
			return auditedRequest(t, ts, http.MethodPost, base+"/release", ids.New().String(), "")
		},
		"moneda desconocida": func() (response, string) {
			return auditedRequest(t, ts, http.MethodPost, base+"/capture", ids.New().String(), strings.Replace(captureBody(10), "COP", "XXX", 1))
		},
		"clave inválida": func() (response, string) {
			return auditedRequest(t, ts, http.MethodPost, base+"/release", strings.Repeat("k", maxIdempotencyKey+1), "")
		},
	}
	for name, send := range cases {
		t.Run(name, func(t *testing.T) {
			resp, correlation := send()

			got := onlyRecord(t, correlation)
			if resp.status/100 != 4 || got.Result != codeOf(t, resp) || !reflect.DeepEqual(got.Targets, []string{hold.Id.String()}) {
				t.Fatalf("respuesta %d %s; registro %+v, se esperaba el rechazo con el hold como target", resp.status, resp.body, got.Record)
			}
		})
	}
}

// TestAnUnauthenticatedCommandIsAudited: un POST sin token recibe 401 y queda registrado, sin actor
// conocido, con el código del problem.
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
		t.Fatalf("respuesta %d; registro %+v, se esperaba el 401 de un anónimo", resp.status, got.Record)
	}
}
