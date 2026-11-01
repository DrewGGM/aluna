//go:build integration

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/outbox"
)

// screeningWorld is an entity whose operator registers parties and whose compliance officers A (the maker)
// and B resolve their screening cases.
type screeningWorld struct {
	operator, a, b *httptest.Server
}

func newScreeningWorld(t *testing.T) screeningWorld {
	t.Helper()
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	entity := ids.New()
	return screeningWorld{
		operator: apiServerWith(t, entity, events),
		a:        apiServerAsRole(t, entity, events, "officer-a", "compliance"),
		b:        apiServerAsRole(t, entity, events, "officer-b", "compliance"),
	}
}

// registered registers, as the operator, a party whose external reference starts with prefix.
func (w screeningWorld) registered(t *testing.T, prefix string) gen.Party {
	t.Helper()
	var p gen.Party
	body := `{"kind": "natural", "external_ref": "` + prefix + ids.New().String() + `", "risk_level": "low", "pep": false}`
	if status := call(t, w.operator, http.MethodPost, "/v1/parties", body, &p); status != http.StatusCreated {
		t.Fatalf("register = %d", status)
	}
	return p
}

// caseOf is the id of the screening case of party p.
func caseOf(t *testing.T, p gen.Party) string {
	t.Helper()
	var id ids.ID
	if err := testPool.QueryRow(t.Context(), `SELECT id FROM compliance.cases WHERE subject_id = $1`, p.Id).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id.String()
}

// requestResolution asks, as A, to resolve case id with resolution.
func (w screeningWorld) requestResolution(t *testing.T, id, resolution string) response {
	t.Helper()
	return postTo(t, w.a, "/v1/screening-cases/"+id+"/resolution", ids.New().String(), `{"resolution": "`+resolution+`"}`)
}

// partyNow reads party p as the operator.
func (w screeningWorld) partyNow(t *testing.T, p gen.Party) gen.Party {
	t.Helper()
	var now gen.Party
	if status := call(t, w.operator, http.MethodGet, "/v1/parties/"+p.Id.String(), "", &now); status != http.StatusOK {
		t.Fatalf("read the party = %d", status)
	}
	return now
}

// approvedByB checks that A cannot approve requested, its own, and has B approve it.
func (w screeningWorld) approvedByB(t *testing.T, requested response) {
	t.Helper()
	if requested.status != http.StatusAccepted {
		t.Fatalf("request the resolution = %d %s, want 202", requested.status, requested.body)
	}
	approval := "/v1/approvals/" + decodeApproval(t, requested).ID + "/approval"
	if r := postTo(t, w.a, approval, ids.New().String(), ""); r.status != http.StatusForbidden {
		t.Fatalf("A approving its own request = %d, want 403", r.status)
	}
	if r := postTo(t, w.b, approval, ids.New().String(), ""); r.status != http.StatusOK || decodeApproval(t, r).Status != "approved" {
		t.Fatalf("B approving = %d %s, want 200 approved", r.status, r.body)
	}
}

// TestAPartyThatMayMatchWaitsForCompliance (FR-42, US-12): a party screening clears is registered in KYC
// pending; one that may match is pending_screening until A asks to clear its case, which A cannot approve
// and B does: the party is then pending, and the case can no longer be resolved.
func TestAPartyThatMayMatchWaitsForCompliance(t *testing.T) {
	w := newScreeningWorld(t)
	clean, matched := w.registered(t, "onb-"), w.registered(t, "match-")
	if clean.KycStatus != gen.KYCStatusPending || matched.KycStatus != gen.KYCStatusPendingScreening {
		t.Fatalf("registered %s and %s, want pending and pending_screening", clean.KycStatus, matched.KycStatus)
	}
	id := caseOf(t, matched)

	w.approvedByB(t, w.requestResolution(t, id, "cleared"))

	if now := w.partyNow(t, matched); now.KycStatus != gen.KYCStatusPending || now.Version != 2 {
		t.Fatalf("the party after its case was cleared = %+v, want version 2 pending", now)
	}
	if again := w.requestResolution(t, id, "confirmed"); again.status != http.StatusConflict || problemCode(t, again) != "case_already_resolved" {
		t.Fatalf("resolving it again = %d %s, want 409 case_already_resolved", again.status, again.body)
	}
}

// TestAResolutionOfAResolvedCaseFails (FR-42): of two resolutions requested for the same case, the one
// approved second leaves its request failed, and the party as the first left it.
func TestAResolutionOfAResolvedCaseFails(t *testing.T) {
	w := newScreeningWorld(t)
	matched := w.registered(t, "match-")
	id := caseOf(t, matched)
	first, second := w.requestResolution(t, id, "cleared"), w.requestResolution(t, id, "confirmed")
	w.approvedByB(t, first)

	r := postTo(t, w.b, "/v1/approvals/"+decodeApproval(t, second).ID+"/approval", ids.New().String(), "")

	if r.status != http.StatusOK || decodeApproval(t, r).Status != "failed" {
		t.Fatalf("approving the second resolution = %d %s, want 200 failed", r.status, r.body)
	}
	if now := w.partyNow(t, matched); now.KycStatus != gen.KYCStatusPending || now.Version != 2 {
		t.Fatalf("the party = %+v, want version 2 pending, as the first resolution left it", now)
	}
}

// TestAConfirmedMatchRejectsTheParty (FR-42): a party whose match is confirmed is rejected.
func TestAConfirmedMatchRejectsTheParty(t *testing.T) {
	w := newScreeningWorld(t)
	matched := w.registered(t, "match-")

	w.approvedByB(t, w.requestResolution(t, caseOf(t, matched), "confirmed"))

	if now := w.partyNow(t, matched); now.KycStatus != gen.KYCStatusRejected {
		t.Fatalf("the party after its match was confirmed = %+v, want rejected", now)
	}
}

// TestAWrongResolutionIsAProblem: a resolution other than cleared or confirmed is 400 invalid_request, a
// case of nowhere 404, and an operator without compliance.screening.resolve is 403.
func TestAWrongResolutionIsAProblem(t *testing.T) {
	w := newScreeningWorld(t)
	id := caseOf(t, w.registered(t, "match-"))
	cases := map[string]struct {
		as             *httptest.Server
		id, resolution string
		status         int
		code           string
	}{
		"an unknown resolution": {w.a, id, "maybe", http.StatusBadRequest, "invalid_request"},
		"a case of nowhere":     {w.a, ids.New().String(), "cleared", http.StatusNotFound, "not_found"},
		"an operator":           {w.operator, id, "cleared", http.StatusForbidden, "forbidden"},
	}
	for name, tc := range cases {
		r := postTo(t, tc.as, "/v1/screening-cases/"+tc.id+"/resolution", ids.New().String(), `{"resolution": "`+tc.resolution+`"}`)
		if r.status != tc.status || problemCode(t, r) != tc.code {
			t.Errorf("%s = %d %s, want %d %s", name, r.status, r.body, tc.status, tc.code)
		}
	}
}
