//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
	"github.com/DrewGGM/aluna/test/harness"
)

// runningAPI is a running aluna api, the token used to talk to it and what signs the tokens of other people of the
// same entity.
type runningAPI struct {
	base, token string
	issuer      *harness.Issuer
	entity      ids.ID
}

// as is the same api with the session of subject, who has role.
func (a runningAPI) as(subject, role string) runningAPI {
	a.token = a.issuer.Sign(subject, a.entity, role)
	return a
}

// newAPI builds aluna, migrates a fresh database with it and starts aluna api on top of it for a new entity.
func newAPI(t *testing.T) runningAPI {
	t.Helper()
	bin := harness.Build(t, "-race")
	dsn, cleanup, err := pgtest.StartEmpty(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	harness.Migrate(t, bin, dsn)
	appDSN, err := pgtest.AppDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return startAPI(t, bin, appDSN, harness.FreeAddr(t), "a secret of the simulated rail, for the tests", ids.New())
}

// startAPI starts aluna api on appDSN with a test OIDC issuer and returns it with the token of an operator of entity.
// The process dies when the test ends.
func startAPI(t *testing.T, bin, appDSN, railAddr, railSecret string, entity ids.ID) runningAPI {
	t.Helper()
	issuer := harness.NewIssuer(t)
	a := runningAPI{base: harness.StartAPI(t, bin, appDSN, railAddr, railSecret, issuer), issuer: issuer, entity: entity}
	return a.as("operator-e2e", "operator")
}

// client keeps a hung process from hanging the test.
var client = &http.Client{Timeout: 10 * time.Second}

// call makes a JSON request to path with a's token and decodes the response into out if it is not nil.
func call(t *testing.T, a runningAPI, method, path, body string, out any) int {
	t.Helper()
	url := a.base + path
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", ids.New().String())
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close() //nolint:errcheck // close in a test
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s = %d: invalid body: %v", method, url, resp.StatusCode, err)
		}
	}
	return resp.StatusCode
}

// expect makes the request as a, fails the test unless it answers want and decodes the response into out if it is not
// nil.
func expect(t *testing.T, a runningAPI, method, path, body string, want int, out any) {
	t.Helper()
	var raw json.RawMessage
	if status := call(t, a, method, path, body, &raw); status != want {
		t.Fatalf("%s %s = %d %s, want %d", method, path, status, raw, want)
	}
	if out == nil {
		return
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
}

// makerChecker has maker request path with body (202 pending), checks that the maker cannot approve it and has checker
// approve it, and returns what the approved command returned.
func makerChecker(t *testing.T, maker, checker runningAPI, path, body string) json.RawMessage {
	t.Helper()
	var request struct{ ID, Status string }
	expect(t, maker, http.MethodPost, path, body, http.StatusAccepted, &request)
	approval := "/v1/approvals/" + request.ID + "/approval"
	var self struct{ Code string }
	expect(t, maker, http.MethodPost, approval, "", http.StatusForbidden, &self)
	var approved struct {
		Status string
		Result json.RawMessage
	}
	expect(t, checker, http.MethodPost, approval, "", http.StatusOK, &approved)
	if request.Status != "pending" || self.Code != "self_approval_forbidden" || approved.Status != "approved" {
		t.Fatalf("%s: requested %s, the maker approving %s, the checker %s %s", path, request.Status, self.Code, approved.Status, approved.Result)
	}
	return approved.Result
}
