//go:build integration

package httpapi

import (
	"net/http"
	"testing"

	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/ids"
)

func newPartyBody() string {
	return `{"kind": "legal", "external_ref": "onb-` + ids.New().String() + `", "risk_level": "low", "pep": false, "consents": {"personal_data": true}}`
}

// TestRegisterAndGetPartyOverHTTP (FR-16): POST /v1/parties registers the party in KYC pending, through the
// real outbox, and GET returns it.
func TestRegisterAndGetPartyOverHTTP(t *testing.T) {
	ts := apiServer(t, ids.New())

	var created gen.Party
	status := call(t, ts, http.MethodPost, "/v1/parties", newPartyBody(), &created)
	var got gen.Party
	getStatus := call(t, ts, http.MethodGet, "/v1/parties/"+created.Id.String(), "", &got)

	if status != http.StatusCreated || created.KycStatus != gen.KYCStatusPending || created.Version != 1 || !created.Consents["personal_data"] {
		t.Fatalf("register = %d %+v", status, created)
	}
	if getStatus != http.StatusOK || got.Id != created.Id || got.ExternalRef != created.ExternalRef || got.Kind != gen.Legal {
		t.Fatalf("get = %d %+v", getStatus, got)
	}
}

// TestRegisterRejectsDuplicateAndInvalidParty: the same external_ref again is 409 party_already_registered;
// invalid data, or a missing pep, is 422 invalid_party.
func TestRegisterRejectsDuplicateAndInvalidParty(t *testing.T) {
	ts := apiServer(t, ids.New())
	body := newPartyBody()
	if status := call(t, ts, http.MethodPost, "/v1/parties", body, nil); status != http.StatusCreated {
		t.Fatalf("register = %d", status)
	}

	var again, invalid, noPEP Problem
	againStatus := call(t, ts, http.MethodPost, "/v1/parties", body, &again)
	invalidStatus := call(t, ts, http.MethodPost, "/v1/parties", `{"kind": "company", "external_ref": "x", "risk_level": "low", "pep": false}`, &invalid)
	noPEPStatus := call(t, ts, http.MethodPost, "/v1/parties", `{"kind": "legal", "external_ref": "no-pep", "risk_level": "low"}`, &noPEP)

	if againStatus != http.StatusConflict || again.Code != "party_already_registered" {
		t.Fatalf("same external_ref = %d %+v, want 409 party_already_registered", againStatus, again)
	}
	if invalidStatus != http.StatusUnprocessableEntity || invalid.Code != "invalid_party" {
		t.Fatalf("unknown kind = %d %+v, want 422 invalid_party", invalidStatus, invalid)
	}
	if noPEPStatus != http.StatusUnprocessableEntity || noPEP.Code != "invalid_party" {
		t.Fatalf("missing pep = %d %+v, want 422 invalid_party: a missing flag is not \"not PEP\"", noPEPStatus, noPEP)
	}
}

// TestAnotherEntitysPartyIsNotFound: a party of another entity is 404, as if it did not exist.
func TestAnotherEntitysPartyIsNotFound(t *testing.T) {
	mine, theirs := apiServer(t, ids.New()), apiServer(t, ids.New())
	var created gen.Party
	if status := call(t, theirs, http.MethodPost, "/v1/parties", newPartyBody(), &created); status != http.StatusCreated {
		t.Fatalf("register = %d", status)
	}

	var p Problem
	status := call(t, mine, http.MethodGet, "/v1/parties/"+created.Id.String(), "", &p)

	if status != http.StatusNotFound || p.Code != "not_found" {
		t.Fatalf("another entity's party = %d %+v, want 404 not_found", status, p)
	}
}
