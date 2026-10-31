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

// testParty is the party of rolesVerifier's principals.
var testParty = ids.New()

// rolesVerifier accepts as the token the principal's list of roles, separated by commas; "none" is a
// principal without roles.
type rolesVerifier struct{}

func (rolesVerifier) Verify(_ context.Context, raw string) (access.Principal, error) {
	p := access.Principal{Subject: "subject-1", LegalEntityID: ids.New(), PartyID: &testParty}
	if raw != "none" {
		p.Roles = strings.Split(raw, ",")
	}
	return p, nil
}

// authorizedServer is a server with authentication and authorization whose routes, those of the contract and
// one that is not in it, answer 200 with the X-Handler header.
func authorizedServer(t *testing.T, a *Authorizer) (*http.Server, *bytes.Buffer) {
	t.Helper()
	reached := func(w http.ResponseWriter, _ *http.Request) { w.Header().Set("X-Handler", "yes") }
	return newTestServer(t, ServerConfig{Verifier: rolesVerifier{}, Authorizer: a}, func(mux *http.ServeMux) {
		for op := range testAuthorizer(t).permissions {
			mux.HandleFunc(op, reached)
		}
		mux.HandleFunc("GET /v1/outside-the-contract", reached)
	})
}

func asRoles(method, path, roles string) *http.Request {
	req := httptest.NewRequest(method, withIDs(path), nil)
	req.Header.Set("Authorization", "Bearer "+roles)
	return req
}

// TestAuthorizationMatrix (FR-38, TH-3, AB-4): the operation runs only if some role of the principal grants
// its permission; otherwise, 403 problem+json. A route that is not in the contract is denied. A channel,
// which only sees what belongs to its party_id, gets 404 for another party's account, as if it did not
// exist, and the handler does not run.
func TestAuthorizationMatrix(t *testing.T) {
	srv, _ := authorizedServer(t, testAuthorizer(t))
	cases := []struct {
		name, method, path, roles string
		status                    int
		code                      string
	}{
		{"operator posts", http.MethodPost, "/v1/transactions", "operator", http.StatusOK, ""},
		{"auditor reads", http.MethodGet, "/v1/transactions/{id}", "auditor", http.StatusOK, ""},
		{"auditor does not post", http.MethodPost, "/v1/transactions", "auditor", http.StatusForbidden, "forbidden"},
		{"finance does not hold", http.MethodPost, "/v1/holds", "finance", http.StatusForbidden, "forbidden"},
		{"unknown role", http.MethodGet, "/v1/ledger-accounts/{id}", "admin", http.StatusForbidden, "forbidden"},
		{"no roles", http.MethodGet, "/v1/ledger-accounts/{id}", "none", http.StatusForbidden, "forbidden"},
		{"route outside the contract", http.MethodGet, "/v1/outside-the-contract", "operator", http.StatusForbidden, "forbidden"},
		{"channel, another party's account", http.MethodGet, "/v1/ledger-accounts/{id}", "channel", http.StatusNotFound, "not_found"},
		{"channel, another party's balance", http.MethodGet, "/v1/ledger-accounts/{id}/balance", "channel", http.StatusNotFound, "not_found"},
		{"channel does not post", http.MethodPost, "/v1/transactions", "channel", http.StatusForbidden, "forbidden"},
		{"channel and operator", http.MethodGet, "/v1/ledger-accounts/{id}", "channel,operator", http.StatusOK, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(srv, asRoles(tc.method, tc.path, tc.roles))

			ran := rec.Header().Get("X-Handler") != ""
			if rec.Code != tc.status || ran != (tc.status == http.StatusOK) {
				t.Fatalf("= %d (handler ran: %v), want %d", rec.Code, ran, tc.status)
			}
			if tc.code != "" {
				if got := decodeProblem(t, rec)["code"]; got != tc.code {
					t.Fatalf("code = %v, want %s", got, tc.code)
				}
			}
		})
	}
}

// TestADenialIsLogged: a denial is logged with the route, the permission, the reason and the roles.
func TestADenialIsLogged(t *testing.T) {
	srv, logs := authorizedServer(t, testAuthorizer(t))

	serve(srv, asRoles(http.MethodPost, "/v1/transactions", "auditor"))

	if strings.Contains(logs.String(), "subject-1") {
		t.Errorf("the denial log carries the sub:\n%s", logs.String())
	}
	for _, want := range []string{`"msg":"permission denied"`, `"route":"POST /v1/transactions"`, `"permission":"ledger.transactions.post"`, `"reason":"no role grants the permission"`, `"roles":"auditor"`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log does not have %s:\n%s", want, logs.String())
		}
	}
}

// TestAnotherPartysResourceIsLogged (AB-4): a channel's 404 on another party's resource leaves the same
// denial log, with its party, to alert on whoever probes other parties' IDs. Today no resource has an owner,
// so everything a channel asks for belongs to someone else; the case of its own (200) comes with the deposit
// accounts (Q-94).
func TestAnotherPartysResourceIsLogged(t *testing.T) {
	srv, logs := authorizedServer(t, testAuthorizer(t))

	serve(srv, asRoles(http.MethodGet, "/v1/ledger-accounts/{id}", "channel"))

	for _, want := range []string{`"msg":"permission denied"`, `"reason":"scope own: no resource has an owner yet"`, `"party_id":"` + testParty.String() + `"`, `"roles":"channel"`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log does not have %s:\n%s", want, logs.String())
		}
	}
}

// TestOnlyCleanPathRedirectsSkipAuthorization: a non-canonical path passes without authorization because the
// mux only redirects it (307, no handler) and the clean request is authorized; one with %2F is canonical for
// the mux, reaches the handler and is authorized.
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
			t.Errorf("auditor's POST %s = %d (handler ran: %v), want %d without handler",
				path, rec.Code, rec.Header().Get("X-Handler") != "", want)
		}
	}
}

// TestWithoutAuthorizerEverythingIsDenied: an authenticated server without an authorizer denies every route;
// the probes keep answering.
func TestWithoutAuthorizerEverythingIsDenied(t *testing.T) {
	srv, _ := authorizedServer(t, nil)

	if rec := serve(srv, asRoles(http.MethodGet, "/v1/ledger-accounts/{id}", "operator")); rec.Code != http.StatusForbidden {
		t.Fatalf("without authorizer = %d, want 403", rec.Code)
	}
	if rec := serve(srv, httptest.NewRequest(http.MethodGet, pathReadyz, nil)); rec.Code != http.StatusOK {
		t.Fatalf("/readyz without authorizer = %d, want 200", rec.Code)
	}
}
