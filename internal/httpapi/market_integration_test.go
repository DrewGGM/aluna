//go:build integration

package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/outbox"
)

// usedMarketYears hands out a year per test, from 4000 on: the parameters are the tenant's, shared by every
// test of the database (also when -count repeats them).
var usedMarketYears atomic.Int64

// marketWorld is an entity with two finance users, A (the maker) and B, and an operator that only reads.
type marketWorld struct {
	a, b, operator *httptest.Server
	year           int64
}

func newMarketWorld(t *testing.T) marketWorld {
	t.Helper()
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	entity := ids.New()
	return marketWorld{
		a:        apiServerAsRole(t, entity, events, "finance-a", "finance"),
		b:        apiServerAsRole(t, entity, events, "finance-b", "finance"),
		operator: apiServerWith(t, entity, events),
		year:     4000 + usedMarketYears.Add(2),
	}
}

// uvt is a load of the UVT for the world's year with value v.
func (w marketWorld) uvt(v string) string {
	return fmt.Sprintf(`{"kind": "uvt", "valid_from": "%d-01-01", "valid_to": "%d-01-01", "value": %q}`, w.year, w.year+1, v)
}

func (w marketWorld) read(t *testing.T, on string) (int, gen.MarketParameterValue) {
	t.Helper()
	var v gen.MarketParameterValue
	status := call(t, w.operator, http.MethodGet, "/v1/market-parameters?kind=uvt&on="+on, "", &v)
	return status, v
}

// TestALoadNeedsAnotherPerson (FR-21): A asks to load the UVT (202, pending, nothing holds yet); A cannot
// approve it; B approves it and the value holds in its year.
func TestALoadNeedsAnotherPerson(t *testing.T) {
	w := newMarketWorld(t)
	requested := postTo(t, w.a, "/v1/market-parameters", ids.New().String(), w.uvt("49799"))
	request := decodeApproval(t, requested)
	on := fmt.Sprintf("%d-06-15", w.year)
	pendingStatus, _ := w.read(t, on)
	path := "/v1/approvals/" + request.ID + "/approval"

	self := postTo(t, w.a, path, ids.New().String(), "")
	approved := postTo(t, w.b, path, ids.New().String(), "")
	status, got := w.read(t, on)

	if requested.status != http.StatusAccepted || request.Kind != marketLoadKind || pendingStatus != http.StatusNotFound {
		t.Fatalf("request = %d %s; read while pending = %d, want 202 and 404", requested.status, requested.body, pendingStatus)
	}
	expectProblem(t, "A approves their own load", self, http.StatusForbidden, "self_approval_forbidden")
	if decoded := decodeApproval(t, approved); approved.status != http.StatusOK || decoded.Status != "approved" {
		t.Fatalf("B approves = %d %s", approved.status, approved.body)
	}
	if status != http.StatusOK || got.Value != "49799" || got.Kind != gen.Uvt {
		t.Fatalf("UVT = %d %+v, want 49799", status, got)
	}
}

// TestALoadIsChecked: an invalid value or a missing date is 422, a period that overlaps a loaded one is
// 409, a load that overlaps when approving is left failed, a day without a value is 404
// no_market_parameter, a read of an unknown kind is 422, and an operator cannot load.
func TestALoadIsChecked(t *testing.T) {
	w := newMarketWorld(t)
	first := decodeApproval(t, postTo(t, w.a, "/v1/market-parameters", ids.New().String(), w.uvt("49799")))
	second := decodeApproval(t, postTo(t, w.a, "/v1/market-parameters", ids.New().String(), w.uvt("50000")))
	if r := postTo(t, w.b, "/v1/approvals/"+first.ID+"/approval", ids.New().String(), ""); r.status != http.StatusOK {
		t.Fatalf("approve the first = %d %s", r.status, r.body)
	}

	invalid := postTo(t, w.a, "/v1/market-parameters", ids.New().String(), w.uvt("1e3"))
	noStart := postTo(t, w.a, "/v1/market-parameters", ids.New().String(), fmt.Sprintf(`{"kind": "smmlv", "valid_to": "%d-01-01", "value": "1"}`, w.year))
	unknown := call(t, w.operator, http.MethodGet, "/v1/market-parameters?kind=inflation&on=2026-01-01", "", nil)
	overlapping := postTo(t, w.a, "/v1/market-parameters", ids.New().String(), w.uvt("51000"))
	stale := postTo(t, w.b, "/v1/approvals/"+second.ID+"/approval", ids.New().String(), "")
	byOperator := postTo(t, w.operator, "/v1/market-parameters", ids.New().String(), w.uvt("49799"))
	missing := call(t, w.operator, http.MethodGet, fmt.Sprintf("/v1/market-parameters?kind=uvt&on=%d-01-01", w.year+1), "", nil)

	expectProblem(t, "a value with an exponent", invalid, http.StatusUnprocessableEntity, "invalid_market_parameter")
	expectProblem(t, "a load without valid_from", noStart, http.StatusUnprocessableEntity, "invalid_market_parameter")
	expectProblem(t, "an overlapping period", overlapping, http.StatusConflict, "market_parameter_overlap")
	if got := decodeApproval(t, stale); stale.status != http.StatusOK || got.Status != "failed" {
		t.Fatalf("approve the overlapping one = %d %s, want 200 failed", stale.status, stale.body)
	}
	if byOperator.status != http.StatusForbidden || missing != http.StatusNotFound || unknown != http.StatusUnprocessableEntity {
		t.Fatalf("operator loads = %d %s, read a day without value = %d, an unknown kind = %d; want 403, 404 and 422",
			byOperator.status, byOperator.body, missing, unknown)
	}
	var result map[string]any
	if err := json.Unmarshal(first.Payload, &result); err != nil || result["value"] != "49799" {
		t.Fatalf("payload of the first = %s (%v), want the loaded value", first.Payload, err)
	}
}
