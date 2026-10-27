//go:build integration

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// response es lo que el cliente ve de una respuesta.
type response struct {
	status      int
	contentType string
	body        []byte
}

// send hace POST path con body y la cabecera Idempotency-Key (omitida si key es nil). No llama
// t.Fatal: se puede usar desde otras goroutines.
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
	defer resp.Body.Close() //nolint:errcheck // cierre en prueba
	b, err := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: b}, err
}

// postWithKey hace POST /v1/transactions con la Idempotency-Key key.
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

// concurrently hace n veces el mismo POST /v1/transactions a la vez y devuelve las respuestas.
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

// allTheSame verifica que todas las respuestas sean idénticas a want.
func allTheSame(t *testing.T, responses []response, want response) {
	t.Helper()
	for i, r := range responses {
		if r.status != want.status || !bytes.Equal(r.body, want.body) {
			t.Errorf("respuesta %d = %d %s, se esperaba %d %s", i, r.status, r.body, want.status, want.body)
		}
	}
}

func problemCode(t *testing.T, r response) string {
	t.Helper()
	var p Problem
	if err := json.Unmarshal(r.body, &p); err != nil || r.contentType != "application/problem+json" {
		t.Fatalf("respuesta %d no es un problem: %s (%v)", r.status, r.body, err)
	}
	return p.Code
}

func balanceOf(t *testing.T, ts *httptest.Server, account string) int64 {
	t.Helper()
	var b struct{ Posted struct{ Amount int64 } }
	if status := call(t, ts, http.MethodGet, "/v1/ledger-accounts/"+account+"/balance", "", &b); status != http.StatusOK {
		t.Fatalf("saldo = %d", status)
	}
	return b.Posted.Amount
}

// newAccounts crea una caja y un depósito en una entidad nueva.
func newAccounts(t *testing.T) (ts *httptest.Server, cash, deposit string) {
	t.Helper()
	ts = apiServer(t, ids.New())
	return ts, createAccount(t, ts, "asset"), createAccount(t, ts, "liability")
}

func TestPostTransactionRequiresAValidIdempotencyKey(t *testing.T) {
	ts, cash, deposit := newAccounts(t)
	body := postingBody(t, cash, deposit, 100, 100, "")
	empty, long, notUTF8 := "", strings.Repeat("k", 256), "\xff\xfeclave"
	cases := map[string]struct {
		key  *string
		code string
	}{
		"sin la cabecera":   {nil, "idempotency_key_required"},
		"vacía":             {&empty, "idempotency_key_required"},
		"de 256 caracteres": {&long, "invalid_request"},
		"que no es UTF-8":   {&notUTF8, "invalid_request"},
	}
	for name, tc := range cases {
		r, err := send(t.Context(), ts, "/v1/transactions", tc.key, body)
		if err != nil || r.status != http.StatusBadRequest || problemCode(t, r) != tc.code {
			t.Errorf("clave %s = %d %s (%v), se esperaba 400 %s", name, r.status, r.body, err, tc.code)
		}
	}
	if r := postWithKey(t, ts, strings.Repeat("ñ", 255), body); r.status != http.StatusCreated {
		t.Errorf("255 caracteres de dos bytes = %d %s, se esperaba 201: el límite es en caracteres", r.status, r.body)
	}
	if got := balanceOf(t, ts, cash); got != 100 {
		t.Errorf("saldo = %d, se esperaba 100: los requests rechazados no asientan", got)
	}
}

// TestReplayReturnsTheOriginalResponse: el reintento recibe el mismo status y el mismo cuerpo, byte a
// byte, aunque el JSON llegue con otro formato, y el dinero se mueve una sola vez.
func TestReplayReturnsTheOriginalResponse(t *testing.T) {
	ts, cash, deposit := newAccounts(t)
	key, body := ids.New().String(), postingBody(t, cash, deposit, 100, 100, "")
	first := postWithKey(t, ts, key, body)
	if first.status != http.StatusCreated || first.contentType != "application/json" {
		t.Fatalf("primer POST = %d %s", first.status, first.body)
	}
	var reformatted bytes.Buffer
	if err := json.Indent(&reformatted, []byte(body), "", "    "); err != nil {
		t.Fatal(err)
	}

	allTheSame(t, []response{postWithKey(t, ts, key, body), postWithKey(t, ts, key, reformatted.String())}, first)

	if got := balanceOf(t, ts, cash); got != 100 {
		t.Errorf("saldo = %d, se esperaba 100: el replay no debe asentar otra vez", got)
	}
}

func TestAnotherPayloadWithTheSameKeyIsRejected(t *testing.T) {
	ts, cash, deposit := newAccounts(t)
	key, ref := ids.New().String(), ids.New().String()
	if r := postWithKey(t, ts, key, postingBody(t, cash, deposit, 100, 100, ref)); r.status != http.StatusCreated {
		t.Fatalf("primer POST = %d %s", r.status, r.body)
	}

	r := postWithKey(t, ts, key, postingBody(t, cash, deposit, 200, 200, ref))

	if r.status != http.StatusUnprocessableEntity || problemCode(t, r) != "idempotency_key_reused" {
		t.Fatalf("otro payload = %d %s, se esperaba 422 idempotency_key_reused", r.status, r.body)
	}
	if got := balanceOf(t, ts, cash); got != 100 {
		t.Errorf("saldo = %d, se esperaba 100", got)
	}
}

// storedBusinessError verifica que el POST con una clave nueva responda status y code, y que el replay
// devuelva esa misma respuesta. Devuelve la clave.
func storedBusinessError(t *testing.T, ts *httptest.Server, body string, status int, code string) string {
	t.Helper()
	key := ids.New().String()
	first := postWithKey(t, ts, key, body)
	if first.status != status || problemCode(t, first) != code {
		t.Fatalf("= %d %s, se esperaba %d %s", first.status, first.body, status, code)
	}
	allTheSame(t, []response{postWithKey(t, ts, key, body)}, first)
	return key
}

// TestInsufficientFundsIsStored: un 4xx de negocio es la respuesta de esa clave, aunque después el
// mismo comando pudiera pasar.
func TestInsufficientFundsIsStored(t *testing.T) {
	ts, cash, deposit := newAccounts(t)
	body := postingBody(t, deposit, cash, 100, 100, "")
	key := storedBusinessError(t, ts, body, http.StatusUnprocessableEntity, "insufficient_funds")
	if r := postWithKey(t, ts, ids.New().String(), postingBody(t, cash, deposit, 1000, 1000, "")); r.status != http.StatusCreated {
		t.Fatalf("fondeo = %d %s", r.status, r.body)
	}

	if r := postWithKey(t, ts, key, body); r.status != http.StatusUnprocessableEntity {
		t.Errorf("replay con fondos = %d %s, se esperaba el 422 guardado", r.status, r.body)
	}
}

// TestDuplicateExternalRefIsStored: una referencia que la entidad ya asentó, con otra clave, es 409
// duplicate_external_ref, y esa respuesta se guarda.
func TestDuplicateExternalRefIsStored(t *testing.T) {
	ts, cash, deposit := newAccounts(t)
	ref := ids.New().String()
	if r := postWithKey(t, ts, ids.New().String(), postingBody(t, cash, deposit, 1, 1, ref)); r.status != http.StatusCreated {
		t.Fatalf("posting con ref = %d %s", r.status, r.body)
	}

	storedBusinessError(t, ts, postingBody(t, cash, deposit, 1, 1, ref), http.StatusConflict, "duplicate_external_ref")
}

func TestAccountNotFoundIsStored(t *testing.T) {
	ts, cash, _ := newAccounts(t)

	storedBusinessError(t, ts, postingBody(t, cash, ids.New().String(), 1, 1, ""), http.StatusNotFound, "not_found")
}

// TestAnUnexpectedErrorIsNotStored: un error que no es de negocio (un 500) sube tal cual y no se
// guarda, así que el reintento con la misma clave ejecuta el comando.
func TestAnUnexpectedErrorIsNotStored(t *testing.T) {
	commands := idempotentCommands{uow: postgres.NewUnitOfWork(testPool), keys: idempotency.NewStore(testPool), entity: ids.New()}
	op := idempotentOperation{method: http.MethodPost, path: "/prueba", success: http.StatusCreated}
	key, down := ids.New().String(), errors.New("la base se cayó")
	failing := func(postgres.Tx) (any, error) { return nil, down }
	succeeding := func(postgres.Tx) (any, error) { return map[string]string{"ok": "sí"}, nil }

	if _, err := commands.run(t.Context(), op, key, "cuerpo", failing); !errors.Is(err, down) {
		t.Fatalf("= %v; el error del comando debe subir tal cual", err)
	}
	resp, err := commands.run(t.Context(), op, key, "cuerpo", succeeding)

	if err != nil || resp.status != http.StatusCreated || string(resp.body) != "{\"ok\":\"sí\"}\n" {
		t.Fatalf("reintento tras un 500 = %+v, %v; se esperaba que corriera el comando", resp, err)
	}
}

// TestARequestInFlightIs409: si otra solicitud con la misma clave no termina dentro del lock_timeout
// del pool, la segunda recibe 409 request_in_progress y no asienta.
func TestARequestInFlightIs409(t *testing.T) {
	entity := ids.New()
	ts := apiServer(t, entity)
	cash, deposit := createAccount(t, ts, "asset"), createAccount(t, ts, "liability")
	key := ids.New().String()
	claimed, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce() // si la prueba falla antes, la primera transacción no queda abierta
	go func() {
		done <- postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
			scope := fmt.Sprintf("%s %s %s", entity, postTransaction.method, postTransaction.path)
			if _, err := idempotency.NewStore(testPool).Claim(t.Context(), tx, scope, key, [32]byte{}); err != nil {
				return err
			}
			close(claimed)
			<-release
			return errors.New("la primera se revierte")
		})
	}()
	select {
	case <-claimed:
	case err := <-done:
		t.Fatalf("la primera no reclamó la clave: %v", err)
	}

	r := postWithKey(t, ts, key, postingBody(t, cash, deposit, 100, 100, ""))
	releaseOnce()
	if err := <-done; err == nil {
		t.Fatal("la primera transacción debía revertirse")
	}

	if r.status != http.StatusConflict || problemCode(t, r) != "request_in_progress" {
		t.Fatalf("en vuelo = %d %s, se esperaba 409 request_in_progress", r.status, r.body)
	}
	if got := balanceOf(t, ts, cash); got != 0 {
		t.Errorf("saldo = %d, se esperaba 0", got)
	}
}

// TestConcurrentRequestsPostOnce: 20 solicitudes a la vez con la misma clave asientan una sola
// transacción, y todas reciben la misma respuesta: las que llegan mientras la primera corre la esperan.
func TestConcurrentRequestsPostOnce(t *testing.T) {
	ts, cash, deposit := newAccounts(t)
	key, ref := ids.New().String(), ids.New().String()

	responses := concurrently(t, 20, ts, key, postingBody(t, cash, deposit, 100, 100, ref))

	if responses[0].status != http.StatusCreated {
		t.Fatalf("respuesta = %d %s, se esperaba 201", responses[0].status, responses[0].body)
	}
	allTheSame(t, responses, responses[0])
	var count int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM ledger.transactions WHERE external_ref = $1`, ref).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("transacciones asentadas = %d, se esperaba exactamente 1", count)
	}
}

// TestConcurrentReplaysAllGetTheResponse: varios reintentos a la vez de una clave ya completada leen
// todos su respuesta; ninguno recibe 409.
func TestConcurrentReplaysAllGetTheResponse(t *testing.T) {
	ts, cash, deposit := newAccounts(t)
	key, body := ids.New().String(), postingBody(t, cash, deposit, 100, 100, "")
	first := postWithKey(t, ts, key, body)

	allTheSame(t, concurrently(t, 30, ts, key, body), first)
}

// TestKeysAreScopedByEntity: dos entidades que comparten la base pueden usar la misma clave.
func TestKeysAreScopedByEntity(t *testing.T) {
	key := ids.New().String()
	for range 2 {
		ts, cash, deposit := newAccounts(t)
		if r := postWithKey(t, ts, key, postingBody(t, cash, deposit, 100, 100, "")); r.status != http.StatusCreated {
			t.Fatalf("la misma clave en otra entidad = %d %s, se esperaba 201", r.status, r.body)
		}
	}
}
