//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DrewGGM/aluna/config"
	"github.com/DrewGGM/aluna/internal/access"
	"github.com/DrewGGM/aluna/internal/approvals"
	"github.com/DrewGGM/aluna/internal/audit"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// testApprovalKind es un tipo de solicitud de prueba: aprobarlo exige ledger.transactions.post, que el
// operador tiene y cumplimiento no (aunque decide solicitudes).
const testApprovalKind = "prueba.ajuste"

// approvalWorld es una entidad con el servicio de solicitudes, cuyo tipo de prueba cuenta sus ejecuciones.
type approvalWorld struct {
	svc    *approvals.Service
	entity ids.ID
	runs   *int
}

func newApprovalWorld(t *testing.T) approvalWorld {
	t.Helper()
	policy, err := access.ParsePolicy(config.Roles)
	if err != nil {
		t.Fatal(err)
	}
	w := approvalWorld{svc: approvals.New(policy), entity: ids.New(), runs: new(int)}
	w.svc.RegisterKind(testApprovalKind, "ledger.transactions.post",
		func(context.Context, postgres.Tx, json.RawMessage, access.Principal) (json.RawMessage, error) {
			*w.runs++
			return json.RawMessage(`{"hecho": true}`), nil
		})
	return w
}

// as es la API de solicitudes con la sesión de subject, de roles roles, en la entidad del mundo.
func (w approvalWorld) as(t *testing.T, subject string, roles ...string) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	uow := postgres.NewUnitOfWork(testPool)
	cfg := ServerConfig{
		Audit:      &CommandAudit{Records: audit.New(), UoW: uow},
		Verifier:   entityVerifier{Subject: subject, LegalEntityID: w.entity, Roles: roles},
		Authorizer: testAuthorizer(t),
	}
	api := API{ApprovalHandlers: NewApprovalHandlers(w.svc, uow, idempotency.NewStore(testPool), testPool)}
	handler := NewServer(cfg, Routes(api, log), log).Handler
	ts := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+testToken)
		handler.ServeHTTP(rw, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// request guarda una solicitud de prueba pedida por operador-a, como lo haría el comando que la crea.
func (w approvalWorld) request(t *testing.T) ids.ID {
	t.Helper()
	var r approvals.Request
	err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		var err error
		r, err = w.svc.Request(t.Context(), tx, testApprovalKind, map[string]any{"monto": 100},
			access.Principal{Subject: "operador-a", LegalEntityID: w.entity, Roles: []string{"operator"}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return r.ID
}

func decodeApproval(t *testing.T, r response) testApproval {
	t.Helper()
	var a testApproval
	if err := json.Unmarshal(r.body, &a); err != nil {
		t.Fatalf("respuesta %d no es una solicitud: %s", r.status, r.body)
	}
	return a
}

// testApproval es lo que la prueba lee de una solicitud en la API.
type testApproval struct {
	ID, Kind, Status, Maker, Checker, Reason string
	Payload, Result                          json.RawMessage
}

// TestTheMakerCannotApproveOverHTTP (AB-5, US-11): el operador A pide; A no puede aprobar (403
// self_approval_forbidden) y B sí: el comando corre una vez con el payload original, el reintento con la
// misma clave recibe la misma respuesta, y aprobar otra vez es 409.
func TestTheMakerCannotApproveOverHTTP(t *testing.T) {
	w := newApprovalWorld(t)
	a, b := w.as(t, "operador-a", "operator"), w.as(t, "operador-b", "operator")
	id := w.request(t)
	path := "/v1/approvals/" + id.String() + "/approval"

	self := postTo(t, a, path, ids.New().String(), "")
	key := ids.New().String()
	approved := postTo(t, b, path, key, "")
	replay := postTo(t, b, path, key, "")
	again := postTo(t, b, path, ids.New().String(), "")

	expectProblem(t, "A aprueba lo suyo", self, http.StatusForbidden, "self_approval_forbidden")
	got := decodeApproval(t, approved)
	if approved.status != http.StatusOK || got.Status != "approved" || got.Maker != "operador-a" || got.Checker != "operador-b" ||
		string(got.Payload) != `{"monto":100}` || string(got.Result) != `{"hecho":true}` {
		t.Fatalf("B aprueba = %d %s", approved.status, approved.body)
	}
	if replay.status != approved.status || string(replay.body) != string(approved.body) {
		t.Fatalf("reintento con la misma clave = %d %s, se esperaba la respuesta guardada", replay.status, replay.body)
	}
	expectProblem(t, "aprobar otra vez", again, http.StatusConflict, "approval_not_pending")
	if *w.runs != 1 {
		t.Fatalf("el comando corrió %d veces, se esperaba 1", *w.runs)
	}
}

// TestRejectionOverHTTP: un rechazo sin motivo es 422; con motivo deja la solicitud rejected sin correr el
// comando, y aparece al listar las rechazadas.
func TestRejectionOverHTTP(t *testing.T) {
	w := newApprovalWorld(t)
	b := w.as(t, "operador-b", "operator")
	id := w.request(t)
	path := "/v1/approvals/" + id.String() + "/rejection"

	noReason, err := send(t.Context(), b, path, nil, `{"reason": ""}`)
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := send(t.Context(), b, path, nil, `{"reason": "monto equivocado"}`)
	if err != nil {
		t.Fatal(err)
	}

	expectProblem(t, "sin motivo", noReason, http.StatusUnprocessableEntity, "rejection_reason_required")
	if got := decodeApproval(t, rejected); rejected.status != http.StatusOK || got.Status != "rejected" || got.Reason != "monto equivocado" || *w.runs != 0 {
		t.Fatalf("rechazo = %d %s; el comando corrió %d veces", rejected.status, rejected.body, *w.runs)
	}
	var list struct{ Items []testApproval }
	if status := getJSON(t, b, "/v1/approvals?status=rejected", &list); status != http.StatusOK || len(list.Items) != 1 || list.Items[0].ID != id.String() {
		t.Fatalf("rechazadas = %d %+v, se esperaba solo %s", status, list.Items, id)
	}
}

// TestAnUnknownStatusIs400: un status que no es de solicitud no se toma como un filtro vacío.
func TestAnUnknownStatusIs400(t *testing.T) {
	b := newApprovalWorld(t).as(t, "operador-b", "operator")

	var problem Problem
	status := getJSON(t, b, "/v1/approvals?status=rechazada", &problem)

	if status != http.StatusBadRequest || problem.Code != "invalid_request" {
		t.Fatalf("estado desconocido = %d %+v, se esperaba 400 invalid_request", status, problem)
	}
}

// TestAnAuditorReadsButDoesNotDecide: el auditor lista las solicitudes pero no las decide (403 forbidden
// de la autorización).
func TestAnAuditorReadsButDoesNotDecide(t *testing.T) {
	w := newApprovalWorld(t)
	auditor := w.as(t, "auditor-1", "auditor")
	id := w.request(t)

	var list struct{ Items []testApproval }
	status := getJSON(t, auditor, "/v1/approvals?status=pending", &list)
	decided := postTo(t, auditor, "/v1/approvals/"+id.String()+"/approval", ids.New().String(), "")

	if status != http.StatusOK || len(list.Items) != 1 {
		t.Fatalf("el auditor lista = %d %+v, se esperaba la pendiente", status, list.Items)
	}
	expectProblem(t, "el auditor aprueba", decided, http.StatusForbidden, "forbidden")
}

// TestTheKindPermissionIsRequired: quien puede decidir solicitudes (approvals.decide) pero no tiene el
// permiso del tipo (compliance no asienta) recibe 403 forbidden y el comando no corre.
func TestTheKindPermissionIsRequired(t *testing.T) {
	w := newApprovalWorld(t)
	compliance := w.as(t, "cumplimiento-1", "compliance")
	id := w.request(t)

	decided := postTo(t, compliance, "/v1/approvals/"+id.String()+"/approval", ids.New().String(), "")

	expectProblem(t, "cumplimiento aprueba un ajuste", decided, http.StatusForbidden, "forbidden")
	if *w.runs != 0 {
		t.Fatalf("el comando corrió %d veces, se esperaba 0", *w.runs)
	}
}

// TestAnApprovalIsAudited (FR-40): la aprobación queda en el registro de auditoría con quien aprobó y la
// solicitud.
func TestAnApprovalIsAudited(t *testing.T) {
	w := newApprovalWorld(t)
	b := w.as(t, "operador-b", "operator")
	id := w.request(t)

	resp, correlation := auditedRequest(t, b, http.MethodPost, "/v1/approvals/"+id.String()+"/approval", ids.New().String(), "")

	got := onlyRecord(t, correlation)
	if resp.status != http.StatusOK || got.Actor != "operador-b" || got.Command != "POST /v1/approvals/{id}/approval" ||
		got.Result != resultOK || len(got.Targets) == 0 || got.Targets[0] != id.String() {
		t.Fatalf("respuesta %d; registro %+v", resp.status, got.Record)
	}
}

// expectProblem verifica que r sea el problem status con code.
func expectProblem(t *testing.T, what string, r response, status int, code string) {
	t.Helper()
	if r.status != status || problemCode(t, r) != code {
		t.Fatalf("%s = %d %s, se esperaba %d %s", what, r.status, r.body, status, code)
	}
}

// getJSON hace GET path y decodifica la respuesta en out.
func getJSON(t *testing.T, ts *httptest.Server, path string, out any) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // cierre en prueba
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}
