//go:build integration

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/outbox"
)

// kycWorld is an entity with two operators, A (the maker, apiServer's) and B, and a party registered by A.
type kycWorld struct {
	a, b  *httptest.Server
	party string
}

func newKYCWorld(t *testing.T) kycWorld {
	t.Helper()
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	entity := ids.New()
	w := kycWorld{a: apiServerWith(t, entity, events), b: apiServerAs(t, entity, events, "operator-b")}
	var created gen.Party
	if status := call(t, w.a, http.MethodPost, "/v1/parties", newPartyBody(), &created); status != http.StatusCreated {
		t.Fatalf("register = %d", status)
	}
	w.party = created.Id.String()
	return w
}

// requestKYC asks, as A, for a change of the party to status; it returns the response.
func (w kycWorld) requestKYC(t *testing.T, status string) response {
	t.Helper()
	return postTo(t, w.a, "/v1/parties/"+w.party+"/kyc-status-changes", ids.New().String(), `{"kyc_status": "`+status+`"}`)
}

// requestedKYC asks for the change as requestKYC and returns the request, which must be pending (202).
func (w kycWorld) requestedKYC(t *testing.T, status string) testApproval {
	t.Helper()
	r := w.requestKYC(t, status)
	if r.status != http.StatusAccepted {
		t.Fatalf("request KYC change = %d %s, want 202", r.status, r.body)
	}
	return decodeApproval(t, r)
}

// TestAKYCChangeNeedsAnotherPerson (FR-16, FR-39): A asks to approve the party's KYC (202, pending) and cannot
// approve it; B approves it and the party has a version 2 in KYC approved.
func TestAKYCChangeNeedsAnotherPerson(t *testing.T) {
	w := newKYCWorld(t)
	request := w.requestedKYC(t, "approved")
	path := "/v1/approvals/" + request.ID + "/approval"
	var pending gen.Party
	if status := call(t, w.a, http.MethodGet, "/v1/parties/"+w.party, "", &pending); status != http.StatusOK || pending.Version != 1 {
		t.Fatalf("party while the request is pending = %d %+v, want version 1", status, pending)
	}

	self := postTo(t, w.a, path, ids.New().String(), "")
	approved := postTo(t, w.b, path, ids.New().String(), "")
	var got gen.Party
	status := call(t, w.a, http.MethodGet, "/v1/parties/"+w.party, "", &got)

	if request.Kind != kycChangeKind || request.Status != "pending" {
		t.Fatalf("request = %+v, want a pending %s", request, kycChangeKind)
	}
	expectProblem(t, "A approves their own change", self, http.StatusForbidden, "self_approval_forbidden")
	expectKYCApproved(t, approved, w.party)
	if status != http.StatusOK || got.KycStatus != gen.KYCStatusApproved || got.Version != 2 {
		t.Fatalf("party = %d %+v, want KYC approved at version 2", status, got)
	}
}

// expectKYCApproved checks that the approval of a change of party to approved succeeded and returned only the
// new state.
func expectKYCApproved(t *testing.T, approved response, party string) {
	t.Helper()
	decoded := decodeApproval(t, approved)
	var result map[string]any
	if err := json.Unmarshal(decoded.Result, &result); err != nil || approved.status != http.StatusOK || decoded.Status != "approved" ||
		!reflect.DeepEqual(result, map[string]any{"party_id": party, "version": 2.0, "kyc_status": "approved"}) {
		t.Fatalf("approve = %d %s; want approved with only the new state as result", approved.status, approved.body)
	}
}

// TestAnInvalidKYCChangeIsRejected: a change the current status does not allow is 422 invalid_kyc_transition
// when requested, and leaves no request.
func TestAnInvalidKYCChangeIsRejected(t *testing.T) {
	w := newKYCWorld(t)

	r := w.requestKYC(t, "suspended")

	expectProblem(t, "pending to suspended", r, http.StatusUnprocessableEntity, "invalid_kyc_transition")
	var list struct{ Items []testApproval }
	if status := getJSON(t, w.a, "/v1/approvals", &list); status != http.StatusOK || len(list.Items) != 0 {
		t.Fatalf("requests = %d %+v, want none", status, list.Items)
	}
}

// approve approves request as B and returns the decided request.
func (w kycWorld) approve(t *testing.T, request testApproval) testApproval {
	t.Helper()
	r := postTo(t, w.b, "/v1/approvals/"+request.ID+"/approval", ids.New().String(), "")
	if r.status != http.StatusOK {
		t.Fatalf("approve %s = %d %s", request.ID, r.status, r.body)
	}
	return decodeApproval(t, r)
}

// TestAStaleKYCChangeFails: a request to suspend an approved party is left failed if, before it is approved,
// the party was suspended and approved again: the transition would fit, but it was decided against an older
// version.
func TestAStaleKYCChangeFails(t *testing.T) {
	w := newKYCWorld(t)
	w.approve(t, w.requestedKYC(t, "approved"))
	stale, suspend := w.requestedKYC(t, "suspended"), w.requestedKYC(t, "suspended")
	w.approve(t, suspend)
	w.approve(t, w.requestedKYC(t, "approved"))

	got := w.approve(t, stale)

	if got.Status != "failed" {
		t.Fatalf("approve the stale request = %+v, want failed", got)
	}
	var party gen.Party
	if status := call(t, w.a, http.MethodGet, "/v1/parties/"+w.party, "", &party); status != http.StatusOK ||
		party.Version != 4 || party.KycStatus != gen.KYCStatusApproved {
		t.Fatalf("party after the failed request = %d %+v, want it still approved at version 4", status, party)
	}
}
