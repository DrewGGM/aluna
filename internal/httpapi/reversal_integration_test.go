//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DrewGGM/aluna/internal/access"
	"github.com/DrewGGM/aluna/internal/approvals"
	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// reversalWorld es una entidad con dos operadores, A (el de apiServer) y B, y una transacción asentada de
// 100 de la caja contra un depósito.
type reversalWorld struct {
	a, b                   *httptest.Server
	cash, deposit, posting string
}

func newReversalWorld(t *testing.T) reversalWorld {
	t.Helper()
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	entity := ids.New()
	w := reversalWorld{a: apiServerWith(t, entity, events), b: apiServerAs(t, entity, events, "operador-b")}
	w.cash, w.deposit = createAccount(t, w.a, "asset"), createAccount(t, w.a, "liability")
	var posted gen.Transaction
	if status := call(t, w.a, http.MethodPost, "/v1/transactions", postingBody(t, w.cash, w.deposit, 100, 100, ""), &posted); status != http.StatusCreated {
		t.Fatalf("asentar = %d", status)
	}
	w.posting = posted.Id.String()
	return w
}

// requestReversal pide, como A, la reversa de la transacción; devuelve la respuesta.
func (w reversalWorld) requestReversal(t *testing.T, body string) response {
	t.Helper()
	return postTo(t, w.a, "/v1/transactions/"+w.posting+"/reversal", ids.New().String(), body)
}

// requested pide la reversa como requestReversal y devuelve la solicitud, que debe quedar pendiente (202).
func (w reversalWorld) requested(t *testing.T) testApproval {
	t.Helper()
	r := w.requestReversal(t, reversalBody)
	if r.status != http.StatusAccepted {
		t.Fatalf("pedir la reversa = %d %s, se esperaba 202", r.status, r.body)
	}
	return decodeApproval(t, r)
}

const reversalBody = `{"reason": "asentada dos veces", "business_date": "` + postingValueDate + `"}`

// TestAReversalNeedsAnotherOperator (FR-3, FR-39): A pide la reversa (202, pendiente) y no puede aprobarla;
// B la aprueba, se asienta la reversa con los entries de signo contrario y los saldos vuelven a cero.
func TestAReversalNeedsAnotherOperator(t *testing.T) {
	w := newReversalWorld(t)

	requested := w.requestReversal(t, reversalBody)
	approval := decodeApproval(t, requested)
	if requested.status != http.StatusAccepted || approval.Status != "pending" || approval.Kind != reversalKind {
		t.Fatalf("pedir la reversa = %d %s, se esperaba 202 pendiente", requested.status, requested.body)
	}
	path := "/v1/approvals/" + approval.ID + "/approval"

	self := postTo(t, w.a, path, ids.New().String(), "")
	approved := postTo(t, w.b, path, ids.New().String(), "")

	expectProblem(t, "A aprueba su reversa", self, http.StatusForbidden, "self_approval_forbidden")
	got := decodeApproval(t, approved)
	var reversal gen.Transaction
	if err := json.Unmarshal(got.Result, &reversal); approved.status != http.StatusOK || got.Status != "approved" || err != nil ||
		len(reversal.Entries) != 2 {
		t.Fatalf("B aprueba = %d %s", approved.status, approved.body)
	}
	if cash, deposit := balanceOf(t, w.a, w.cash), balanceOf(t, w.a, w.deposit); cash != 0 || deposit != 0 {
		t.Fatalf("saldos tras la reversa = %d y %d, se esperaba 0 y 0", cash, deposit)
	}
}

// TestAReversalTheLedgerRejectsFails: si al aprobarse el ledger rechaza la reversa (la transacción ya
// tiene una), la solicitud queda failed y no se asienta nada más.
func TestAReversalTheLedgerRejectsFails(t *testing.T) {
	w := newReversalWorld(t)
	first, second := w.requested(t), w.requested(t)
	if r := postTo(t, w.b, "/v1/approvals/"+first.ID+"/approval", ids.New().String(), ""); r.status != http.StatusOK {
		t.Fatalf("aprobar la primera = %d %s", r.status, r.body)
	}

	again := postTo(t, w.b, "/v1/approvals/"+second.ID+"/approval", ids.New().String(), "")

	if got := decodeApproval(t, again); again.status != http.StatusOK || got.Status != "failed" {
		t.Fatalf("aprobar la segunda = %d %s, se esperaba 200 failed", again.status, again.body)
	}
	if cash := balanceOf(t, w.a, w.cash); cash != 0 {
		t.Fatalf("saldo de la caja = %d, se esperaba 0: la segunda reversa no debía asentarse", cash)
	}
}

// TestAReversalIsNotReversed: la reversa de una reversa se rechaza al pedirla (409 reversal_of_reversal):
// aprobarla volvería a aplicar la original sin que el checker lo viera.
func TestAReversalIsNotReversed(t *testing.T) {
	w := newReversalWorld(t)
	first := w.requested(t)
	approved := decodeApproval(t, postTo(t, w.b, "/v1/approvals/"+first.ID+"/approval", ids.New().String(), ""))
	var reversal gen.Transaction
	if err := json.Unmarshal(approved.Result, &reversal); err != nil || approved.Status != "approved" {
		t.Fatalf("aprobar la reversa = %+v (%v)", approved, err)
	}

	again := postTo(t, w.a, "/v1/transactions/"+reversal.Id.String()+"/reversal", ids.New().String(), reversalBody)

	expectProblem(t, "reversa de la reversa", again, http.StatusConflict, "reversal_of_reversal")
}

// TestAnUnexpectedLedgerErrorIsNotARejection: un error del ledger que no es una regla de negocio (aquí, el
// contexto cancelado) sube sin envolver en ErrCommandRejected: la solicitud no queda failed.
func TestAnUnexpectedLedgerErrorIsNotARejection(t *testing.T) {
	exec := reverseOnApproval(ledger.New(calendar.New(), failingEvents{}))
	payload, err := json.Marshal(reversalRequest{TransactionID: ids.New(), Reason: "prueba"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err = postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		_, err := exec(ctx, tx, payload, access.Principal{Subject: "operador-b", LegalEntityID: ids.New()})
		return err
	})

	if err == nil || errors.Is(err, approvals.ErrCommandRejected) {
		t.Fatalf("= %v, se esperaba un error que no fuera un rechazo", err)
	}
}

// TestARequestForAReversalIsValidated: pedir la reversa de una transacción que no existe, o que es de otra
// entidad, es 404, y sin motivo es 422; ninguna deja una solicitud.
func TestARequestForAReversalIsValidated(t *testing.T) {
	w, other := newReversalWorld(t), newReversalWorld(t)

	unknown := postTo(t, w.a, "/v1/transactions/"+ids.New().String()+"/reversal", ids.New().String(), reversalBody)
	foreign := postTo(t, w.a, "/v1/transactions/"+other.posting+"/reversal", ids.New().String(), reversalBody)
	noReason := w.requestReversal(t, `{"reason": "  "}`)

	expectProblem(t, "transacción desconocida", unknown, http.StatusNotFound, "not_found")
	expectProblem(t, "transacción de otra entidad", foreign, http.StatusNotFound, "not_found")
	expectProblem(t, "sin motivo", noReason, http.StatusUnprocessableEntity, "invalid_posting")
	var list struct{ Items []testApproval }
	if status := getJSON(t, w.a, "/v1/approvals", &list); status != http.StatusOK || len(list.Items) != 0 {
		t.Fatalf("solicitudes = %d %+v, se esperaba ninguna", status, list.Items)
	}
}
