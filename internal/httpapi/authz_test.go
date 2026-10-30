package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DrewGGM/aluna/internal/access"
	"github.com/DrewGGM/aluna/internal/ids"
)

// testParty es la party de los principales de rolesVerifier.
var testParty = ids.New()

// rolesVerifier acepta como token la lista de roles del principal, separados por comas; "ninguno" es un
// principal sin roles.
type rolesVerifier struct{}

func (rolesVerifier) Verify(_ context.Context, raw string) (access.Principal, error) {
	p := access.Principal{Subject: "sujeto-1", LegalEntityID: ids.New(), PartyID: &testParty}
	if raw != "ninguno" {
		p.Roles = strings.Split(raw, ",")
	}
	return p, nil
}

// authorizedServer es un servidor con autenticación y autorización cuyas rutas, las del contrato y una que
// no está en él, responden 200 con el header X-Handler.
func authorizedServer(t *testing.T, a *Authorizer) (*http.Server, *bytes.Buffer) {
	t.Helper()
	reached := func(w http.ResponseWriter, _ *http.Request) { w.Header().Set("X-Handler", "sí") }
	return newTestServer(t, ServerConfig{Verifier: rolesVerifier{}, Authorizer: a}, func(mux *http.ServeMux) {
		for op := range testAuthorizer(t).permissions {
			mux.HandleFunc(op, reached)
		}
		mux.HandleFunc("GET /v1/fuera-del-contrato", reached)
	})
}

func asRoles(method, path, roles string) *http.Request {
	req := httptest.NewRequest(method, withIDs(path), nil)
	req.Header.Set("Authorization", "Bearer "+roles)
	return req
}

// TestAuthorizationMatrix (FR-38, TH-3, AB-4): la operación corre solo si algún rol del principal da su
// permiso; si no, 403 problem+json. Una ruta que no está en el contrato se niega. Un canal, que solo ve lo
// de su party_id, recibe 404 por la cuenta de otra party, como si no existiera, y el handler no corre.
func TestAuthorizationMatrix(t *testing.T) {
	srv, _ := authorizedServer(t, testAuthorizer(t))
	cases := []struct {
		name, method, path, roles string
		status                    int
		code                      string
	}{
		{"operador asienta", http.MethodPost, "/v1/transactions", "operator", http.StatusOK, ""},
		{"auditor lee", http.MethodGet, "/v1/transactions/{id}", "auditor", http.StatusOK, ""},
		{"auditor no asienta", http.MethodPost, "/v1/transactions", "auditor", http.StatusForbidden, "forbidden"},
		{"finanzas no retiene", http.MethodPost, "/v1/holds", "finance", http.StatusForbidden, "forbidden"},
		{"rol desconocido", http.MethodGet, "/v1/ledger-accounts/{id}", "admin", http.StatusForbidden, "forbidden"},
		{"sin roles", http.MethodGet, "/v1/ledger-accounts/{id}", "ninguno", http.StatusForbidden, "forbidden"},
		{"ruta fuera del contrato", http.MethodGet, "/v1/fuera-del-contrato", "operator", http.StatusForbidden, "forbidden"},
		{"canal, cuenta de otra party", http.MethodGet, "/v1/ledger-accounts/{id}", "channel", http.StatusNotFound, "not_found"},
		{"canal, saldo de otra party", http.MethodGet, "/v1/ledger-accounts/{id}/balance", "channel", http.StatusNotFound, "not_found"},
		{"canal no asienta", http.MethodPost, "/v1/transactions", "channel", http.StatusForbidden, "forbidden"},
		{"canal y operador", http.MethodGet, "/v1/ledger-accounts/{id}", "channel,operator", http.StatusOK, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(srv, asRoles(tc.method, tc.path, tc.roles))

			ran := rec.Header().Get("X-Handler") != ""
			if rec.Code != tc.status || ran != (tc.status == http.StatusOK) {
				t.Fatalf("= %d (corrió el handler: %v), se esperaba %d", rec.Code, ran, tc.status)
			}
			if tc.code != "" {
				if got := decodeProblem(t, rec)["code"]; got != tc.code {
					t.Fatalf("code = %v, se esperaba %s", got, tc.code)
				}
			}
		})
	}
}

// TestADenialIsLogged: una denegación queda en log con la ruta, el permiso, la razón y los roles.
func TestADenialIsLogged(t *testing.T) {
	srv, logs := authorizedServer(t, testAuthorizer(t))

	serve(srv, asRoles(http.MethodPost, "/v1/transactions", "auditor"))

	if strings.Contains(logs.String(), "sujeto-1") {
		t.Errorf("el log de la denegación lleva el sub:\n%s", logs.String())
	}
	for _, want := range []string{`"msg":"permiso denegado"`, `"route":"POST /v1/transactions"`, `"permission":"ledger.transactions.post"`, `"reason":"ningún rol da el permiso"`, `"roles":"auditor"`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("el log no tiene %s:\n%s", want, logs.String())
		}
	}
}

// TestAnotherPartysResourceIsLogged (AB-4): el 404 de un canal sobre un recurso ajeno deja el mismo log de
// denegación, con su party, para alertar sobre quien prueba IDs ajenos. Hoy ningún recurso tiene dueño, así
// que todo lo de un canal es ajeno; el caso de lo propio (200) llega con las cuentas de depósito (Q-94).
func TestAnotherPartysResourceIsLogged(t *testing.T) {
	srv, logs := authorizedServer(t, testAuthorizer(t))

	serve(srv, asRoles(http.MethodGet, "/v1/ledger-accounts/{id}", "channel"))

	for _, want := range []string{`"msg":"permiso denegado"`, `"reason":"alcance own: ningún recurso tiene dueño todavía"`, `"party_id":"` + testParty.String() + `"`, `"roles":"channel"`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("el log no tiene %s:\n%s", want, logs.String())
		}
	}
}

// TestOnlyCleanPathRedirectsSkipAuthorization: una ruta no canónica pasa sin autorizar porque el mux solo
// la redirige (307, sin handler) y el request limpio se autoriza; una con %2F es canónica para el mux, llega
// al handler y se autoriza.
func TestOnlyCleanPathRedirectsSkipAuthorization(t *testing.T) {
	srv, _ := authorizedServer(t, testAuthorizer(t))
	for path, want := range map[string]int{
		"/v1//transactions":            http.StatusTemporaryRedirect,
		"/v1/./transactions":           http.StatusTemporaryRedirect,
		"/v1/x/../transactions":        http.StatusTemporaryRedirect,
		"/v1/holds/a%2F..%2Fb/capture": http.StatusForbidden,
		"/v1/transactions":             http.StatusForbidden,
	} {
		rec := serve(srv, asRoles(http.MethodPost, path, "auditor"))

		if rec.Code != want || rec.Header().Get("X-Handler") != "" {
			t.Errorf("POST %s del auditor = %d (corrió el handler: %v), se esperaba %d sin handler",
				path, rec.Code, rec.Header().Get("X-Handler") != "", want)
		}
	}
}

// TestWithoutAuthorizerEverythingIsDenied: un servidor autenticado sin autorizador niega toda ruta; los
// probes siguen respondiendo.
func TestWithoutAuthorizerEverythingIsDenied(t *testing.T) {
	srv, _ := authorizedServer(t, nil)

	if rec := serve(srv, asRoles(http.MethodGet, "/v1/ledger-accounts/{id}", "operator")); rec.Code != http.StatusForbidden {
		t.Fatalf("sin autorizador = %d, se esperaba 403", rec.Code)
	}
	if rec := serve(srv, httptest.NewRequest(http.MethodGet, pathReadyz, nil)); rec.Code != http.StatusOK {
		t.Fatalf("/readyz sin autorizador = %d, se esperaba 200", rec.Code)
	}
}
