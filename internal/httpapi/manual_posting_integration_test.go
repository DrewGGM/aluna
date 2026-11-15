//go:build integration

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/outbox"
)

// manualApprovalWorld is an entity whose manual postings of 100 COP or more go through maker-checker, with
// operators A and B and a cash account and a deposit.
type manualApprovalWorld struct {
	a, b          *httptest.Server
	cash, deposit string
}

func newManualApprovalWorld(t *testing.T) manualApprovalWorld {
	t.Helper()
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	entity, thresholds := ids.New(), ManualPostingThresholds{"COP": 100}
	w := manualApprovalWorld{
		a: apiServerConfigured(t, entity, events, testSubject, testRole, thresholds),
		b: apiServerConfigured(t, entity, events, "operator-b", testRole, thresholds),
	}
	w.cash, w.deposit = createAccount(t, w.a, "asset"), createAccount(t, w.a, "liability")
	return w
}

// TestAManualPostingOverTheThresholdNeedsAnotherOperator (Q-36, ADR-0019): a posting that reaches the threshold is
// a pending ledger.manual_posting request that posts nothing; A cannot approve it, and B's approval posts it with B
// as its actor.
func TestAManualPostingOverTheThresholdNeedsAnotherOperator(t *testing.T) {
	w := newManualApprovalWorld(t)

	requested := postTo(t, w.a, "/v1/transactions", ids.New().String(), postingBody(t, w.cash, w.deposit, 100, 100, ""))

	approval := decodeApproval(t, requested)
	if requested.status != http.StatusAccepted || approval.Status != "pending" || approval.Kind != manualPostingKind {
		t.Fatalf("a posting of 100 = %d %s, want 202 pending", requested.status, requested.body)
	}
	if cash := balanceOf(t, w.a, w.cash); cash != 0 {
		t.Fatalf("cash = %d before the approval, want 0", cash)
	}
	path := "/v1/approvals/" + approval.ID + "/approval"
	expectProblem(t, "A approves its own posting", postTo(t, w.a, path, ids.New().String(), ""), http.StatusForbidden, "self_approval_forbidden")
	approved := decodeApproval(t, postTo(t, w.b, path, ids.New().String(), ""))
	var posted gen.Transaction
	if err := json.Unmarshal(approved.Result, &posted); err != nil || approved.Status != "approved" {
		t.Fatalf("B approves = %+v (%v)", approved, err)
	}
	if cash := balanceOf(t, w.a, w.cash); cash != 100 {
		t.Fatalf("cash = %d after the approval, want 100", cash)
	}
	expectPostedThroughAPI(t, ids.ID(posted.Id), "operator-b")
}

// TestAManualPostingUnderTheThresholdPostsAtOnce: one below the threshold is posted (201), as without one.
func TestAManualPostingUnderTheThresholdPostsAtOnce(t *testing.T) {
	w := newManualApprovalWorld(t)

	r := postTo(t, w.a, "/v1/transactions", ids.New().String(), postingBody(t, w.cash, w.deposit, 99, 99, ""))

	if r.status != http.StatusCreated || balanceOf(t, w.a, w.cash) != 99 {
		t.Fatalf("a posting of 99 = %d %s, want 201 and posted", r.status, r.body)
	}
}

// TestAManualPostingTheLedgerRefusesIsRefusedWhenRequested: one over the threshold that the ledger would refuse
// (unbalanced) is refused when it is requested, and leaves no request.
func TestAManualPostingTheLedgerRefusesIsRefusedWhenRequested(t *testing.T) {
	w := newManualApprovalWorld(t)

	r := postTo(t, w.a, "/v1/transactions", ids.New().String(), postingBody(t, w.cash, w.deposit, 150, 100, ""))

	expectProblem(t, "an unbalanced posting over the threshold", r, http.StatusUnprocessableEntity, "unbalanced_transaction")
	var list struct{ Items []testApproval }
	if status := getJSON(t, w.a, "/v1/approvals?status=pending", &list); status != http.StatusOK || len(list.Items) != 0 {
		t.Fatalf("pending requests = %d %+v, want none", status, list.Items)
	}
}

// TestARetriedManualPostingRequestIsTheSame: the same request with the same Idempotency-Key answers the same 202 and
// leaves one request; another key with a reference already posted is 409 duplicate_external_ref when requested.
func TestARetriedManualPostingRequestIsTheSame(t *testing.T) {
	w := newManualApprovalWorld(t)
	key, ref := ids.New().String(), ids.New().String()
	body := postingBody(t, w.cash, w.deposit, 150, 150, ref)

	first, again := postTo(t, w.a, "/v1/transactions", key, body), postTo(t, w.a, "/v1/transactions", key, body)

	if first.status != http.StatusAccepted || again.status != http.StatusAccepted || string(first.body) != string(again.body) {
		t.Fatalf("request and retry = %d %s and %d %s, want the same 202", first.status, first.body, again.status, again.body)
	}
	var list struct{ Items []testApproval }
	if status := getJSON(t, w.a, "/v1/approvals?status=pending", &list); status != http.StatusOK || len(list.Items) != 1 {
		t.Fatalf("pending requests = %d %+v, want one", status, list.Items)
	}
	if r := postTo(t, w.a, "/v1/transactions", ids.New().String(), postingBody(t, w.cash, w.deposit, 50, 50, ref)); r.status != http.StatusCreated {
		t.Fatalf("a posting of 50 with the reference = %d %s, want 201", r.status, r.body)
	}
	expectProblem(t, "a request whose reference is posted", postTo(t, w.a, "/v1/transactions", ids.New().String(), body),
		http.StatusConflict, "duplicate_external_ref")
}
