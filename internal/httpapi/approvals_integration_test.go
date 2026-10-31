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

// testApprovalKind is a test request kind: approving it requires ledger.transactions.post, which the operator
// has and compliance does not (although it decides requests).
const testApprovalKind = "test.adjustment"

// approvalWorld is an entity with the requests service, whose test kind counts its runs.
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
			return json.RawMessage(`{"done": true}`), nil
		})
	return w
}

// as is the requests API with the session of subject, with roles roles, in the world's entity.
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

// request stores a test request made by operator-a, as the command that creates it would.
func (w approvalWorld) request(t *testing.T) ids.ID {
	t.Helper()
	var r approvals.Request
	err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		var err error
		r, err = w.svc.Request(t.Context(), tx, testApprovalKind, map[string]any{"amount": 100},
			access.Principal{Subject: "operator-a", LegalEntityID: w.entity, Roles: []string{"operator"}})
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
		t.Fatalf("response %d is not a request: %s", r.status, r.body)
	}
	return a
}

// testApproval is what the test reads from a request in the API.
type testApproval struct {
	ID, Kind, Status, Maker, Checker, Reason string
	Payload, Result                          json.RawMessage
}

// TestTheMakerCannotApproveOverHTTP (AB-5, US-11): operator A requests; A cannot approve (403
// self_approval_forbidden) and B can: the command runs once with the original payload, the retry with the
// same key receives the same response, and approving again is 409.
func TestTheMakerCannotApproveOverHTTP(t *testing.T) {
	w := newApprovalWorld(t)
	a, b := w.as(t, "operator-a", "operator"), w.as(t, "operator-b", "operator")
	id := w.request(t)
	path := "/v1/approvals/" + id.String() + "/approval"

	self := postTo(t, a, path, ids.New().String(), "")
	key := ids.New().String()
	approved := postTo(t, b, path, key, "")
	replay := postTo(t, b, path, key, "")
	again := postTo(t, b, path, ids.New().String(), "")

	expectProblem(t, "A approves its own", self, http.StatusForbidden, "self_approval_forbidden")
	got := decodeApproval(t, approved)
	if approved.status != http.StatusOK || got.Status != "approved" || got.Maker != "operator-a" || got.Checker != "operator-b" ||
		string(got.Payload) != `{"amount":100}` || string(got.Result) != `{"done":true}` {
		t.Fatalf("B approves = %d %s", approved.status, approved.body)
	}
	if replay.status != approved.status || string(replay.body) != string(approved.body) {
		t.Fatalf("retry with the same key = %d %s, want the stored response", replay.status, replay.body)
	}
	expectProblem(t, "approve again", again, http.StatusConflict, "approval_not_pending")
	if *w.runs != 1 {
		t.Fatalf("the command ran %d times, want 1", *w.runs)
	}
}

// TestRejectionOverHTTP: a rejection without a reason is 422; with a reason it leaves the request rejected
// without running the command, and it shows up when listing the rejected ones.
func TestRejectionOverHTTP(t *testing.T) {
	w := newApprovalWorld(t)
	b := w.as(t, "operator-b", "operator")
	id := w.request(t)
	path := "/v1/approvals/" + id.String() + "/rejection"

	noReason, err := send(t.Context(), b, path, nil, `{"reason": ""}`)
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := send(t.Context(), b, path, nil, `{"reason": "wrong amount"}`)
	if err != nil {
		t.Fatal(err)
	}

	expectProblem(t, "no reason", noReason, http.StatusUnprocessableEntity, "rejection_reason_required")
	if got := decodeApproval(t, rejected); rejected.status != http.StatusOK || got.Status != "rejected" || got.Reason != "wrong amount" || *w.runs != 0 {
		t.Fatalf("rejection = %d %s; the command ran %d times", rejected.status, rejected.body, *w.runs)
	}
	var list struct{ Items []testApproval }
	if status := getJSON(t, b, "/v1/approvals?status=rejected", &list); status != http.StatusOK || len(list.Items) != 1 || list.Items[0].ID != id.String() {
		t.Fatalf("rejected = %d %+v, want only %s", status, list.Items, id)
	}
}

// TestAnUnknownStatusIs400: a status that is not a request status is not taken as an empty filter.
func TestAnUnknownStatusIs400(t *testing.T) {
	b := newApprovalWorld(t).as(t, "operator-b", "operator")

	var problem Problem
	status := getJSON(t, b, "/v1/approvals?status=declined", &problem)

	if status != http.StatusBadRequest || problem.Code != "invalid_request" {
		t.Fatalf("unknown status = %d %+v, want 400 invalid_request", status, problem)
	}
}

// TestAnAuditorReadsButDoesNotDecide: the auditor lists the requests but does not decide them (403 forbidden
// from authorization).
func TestAnAuditorReadsButDoesNotDecide(t *testing.T) {
	w := newApprovalWorld(t)
	auditor := w.as(t, "auditor-1", "auditor")
	id := w.request(t)

	var list struct{ Items []testApproval }
	status := getJSON(t, auditor, "/v1/approvals?status=pending", &list)
	decided := postTo(t, auditor, "/v1/approvals/"+id.String()+"/approval", ids.New().String(), "")

	if status != http.StatusOK || len(list.Items) != 1 {
		t.Fatalf("auditor lists = %d %+v, want the pending one", status, list.Items)
	}
	expectProblem(t, "the auditor approves", decided, http.StatusForbidden, "forbidden")
}

// TestTheKindPermissionIsRequired: whoever can decide requests (approvals.decide) but lacks the kind's
// permission (compliance does not post) gets 403 forbidden and the command does not run.
func TestTheKindPermissionIsRequired(t *testing.T) {
	w := newApprovalWorld(t)
	compliance := w.as(t, "compliance-1", "compliance")
	id := w.request(t)

	decided := postTo(t, compliance, "/v1/approvals/"+id.String()+"/approval", ids.New().String(), "")

	expectProblem(t, "compliance approves an adjustment", decided, http.StatusForbidden, "forbidden")
	if *w.runs != 0 {
		t.Fatalf("the command ran %d times, want 0", *w.runs)
	}
}

// TestAnApprovalIsAudited (FR-40): the approval is in the audit record with the approver and the request.
func TestAnApprovalIsAudited(t *testing.T) {
	w := newApprovalWorld(t)
	b := w.as(t, "operator-b", "operator")
	id := w.request(t)

	resp, correlation := auditedRequest(t, b, http.MethodPost, "/v1/approvals/"+id.String()+"/approval", ids.New().String(), "")

	got := onlyRecord(t, correlation)
	if resp.status != http.StatusOK || got.Actor != "operator-b" || got.Command != "POST /v1/approvals/{id}/approval" ||
		got.Result != resultOK || len(got.Targets) == 0 || got.Targets[0] != id.String() {
		t.Fatalf("response %d; record %+v", resp.status, got.Record)
	}
}

// expectProblem checks that r is the problem status with code.
func expectProblem(t *testing.T, what string, r response, status int, code string) {
	t.Helper()
	if r.status != status || problemCode(t, r) != code {
		t.Fatalf("%s = %d %s, want %d %s", what, r.status, r.body, status, code)
	}
}

// getJSON does GET path and decodes the response into out.
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
	defer resp.Body.Close() //nolint:errcheck // close in a test
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}
