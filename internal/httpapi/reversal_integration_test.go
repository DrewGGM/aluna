//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DrewGGM/aluna/internal/access"
	"github.com/DrewGGM/aluna/internal/approvals"
	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// reversalWorld is an entity with two operators, A (apiServer's) and B, and a posted transaction of 100 from
// the cash account against a deposit.
type reversalWorld struct {
	a, b                   *httptest.Server
	cash, deposit, posting string
}

func newReversalWorld(t *testing.T) reversalWorld {
	t.Helper()
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	entity := ids.New()
	w := reversalWorld{a: apiServerWith(t, entity, events), b: apiServerAs(t, entity, events, "operator-b")}
	w.cash, w.deposit = createAccount(t, w.a, "asset"), createAccount(t, w.a, "liability")
	var posted gen.Transaction
	if status := call(t, w.a, http.MethodPost, "/v1/transactions", postingBody(t, w.cash, w.deposit, 100, 100, ""), &posted); status != http.StatusCreated {
		t.Fatalf("post = %d", status)
	}
	w.posting = posted.Id.String()
	return w
}

// requestReversal requests, as A, the reversal of the transaction; it returns the response.
func (w reversalWorld) requestReversal(t *testing.T, body string) response {
	t.Helper()
	return postTo(t, w.a, "/v1/transactions/"+w.posting+"/reversal", ids.New().String(), body)
}

// requested requests the reversal like requestReversal and returns the request, which must be pending (202).
func (w reversalWorld) requested(t *testing.T) testApproval {
	t.Helper()
	r := w.requestReversal(t, reversalBody)
	if r.status != http.StatusAccepted {
		t.Fatalf("request reversal = %d %s, want 202", r.status, r.body)
	}
	return decodeApproval(t, r)
}

const reversalBody = `{"reason": "posted twice", "business_date": "` + postingValueDate + `"}`

// TestAReversalNeedsAnotherOperator (FR-3, FR-39): A requests the reversal (202, pending) and cannot approve
// it; B approves it, the reversal is posted with entries of opposite sign and the balances return to zero.
func TestAReversalNeedsAnotherOperator(t *testing.T) {
	w := newReversalWorld(t)

	requested := w.requestReversal(t, reversalBody)
	approval := decodeApproval(t, requested)
	if requested.status != http.StatusAccepted || approval.Status != "pending" || approval.Kind != reversalKind {
		t.Fatalf("request reversal = %d %s, want 202 pending", requested.status, requested.body)
	}
	path := "/v1/approvals/" + approval.ID + "/approval"

	self := postTo(t, w.a, path, ids.New().String(), "")
	approved := postTo(t, w.b, path, ids.New().String(), "")

	expectProblem(t, "A approves its own reversal", self, http.StatusForbidden, "self_approval_forbidden")
	got := decodeApproval(t, approved)
	var reversal gen.Transaction
	if err := json.Unmarshal(got.Result, &reversal); approved.status != http.StatusOK || got.Status != "approved" || err != nil ||
		len(reversal.Entries) != 2 {
		t.Fatalf("B approves = %d %s", approved.status, approved.body)
	}
	if cash, deposit := balanceOf(t, w.a, w.cash), balanceOf(t, w.a, w.deposit); cash != 0 || deposit != 0 {
		t.Fatalf("balances after the reversal = %d and %d, want 0 and 0", cash, deposit)
	}
}

// TestAReversalTheLedgerRejectsFails: if on approval the ledger rejects the reversal (the transaction already
// has one), the request ends up failed and nothing else is posted.
func TestAReversalTheLedgerRejectsFails(t *testing.T) {
	w := newReversalWorld(t)
	first, second := w.requested(t), w.requested(t)
	if r := postTo(t, w.b, "/v1/approvals/"+first.ID+"/approval", ids.New().String(), ""); r.status != http.StatusOK {
		t.Fatalf("approve the first = %d %s", r.status, r.body)
	}

	again := postTo(t, w.b, "/v1/approvals/"+second.ID+"/approval", ids.New().String(), "")

	if got := decodeApproval(t, again); again.status != http.StatusOK || got.Status != "failed" {
		t.Fatalf("approve the second = %d %s, want 200 failed", again.status, again.body)
	}
	if cash := balanceOf(t, w.a, w.cash); cash != 0 {
		t.Fatalf("cash balance = %d, want 0: the second reversal must not be posted", cash)
	}
}

// TestAReversalIsNotReversed: the reversal of a reversal is rejected when requested (409
// reversal_of_reversal): approving it would apply the original again without the checker seeing it.
func TestAReversalIsNotReversed(t *testing.T) {
	w := newReversalWorld(t)
	first := w.requested(t)
	approved := decodeApproval(t, postTo(t, w.b, "/v1/approvals/"+first.ID+"/approval", ids.New().String(), ""))
	var reversal gen.Transaction
	if err := json.Unmarshal(approved.Result, &reversal); err != nil || approved.Status != "approved" {
		t.Fatalf("approve the reversal = %+v (%v)", approved, err)
	}

	again := postTo(t, w.a, "/v1/transactions/"+reversal.Id.String()+"/reversal", ids.New().String(), reversalBody)

	expectProblem(t, "reversal of the reversal", again, http.StatusConflict, "reversal_of_reversal")
}

// TestAnUnexpectedLedgerErrorIsNotARejection: a ledger error that is not a business rule (here, the canceled
// context) goes up without being wrapped in ErrCommandRejected: the request does not end up failed.
func TestAnUnexpectedLedgerErrorIsNotARejection(t *testing.T) {
	exec := reverseOnApproval(ledger.New(calendar.New(), failingEvents{}))
	payload, err := json.Marshal(reversalRequest{TransactionID: ids.New(), Reason: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err = postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		_, err := exec(ctx, tx, payload, access.Principal{Subject: "operator-b", LegalEntityID: ids.New()})
		return err
	})

	if err == nil || errors.Is(err, approvals.ErrCommandRejected) {
		t.Fatalf("= %v, want an error that is not a rejection", err)
	}
}

// TestARequestForAReversalIsValidated: requesting the reversal of a transaction that does not exist, or that
// belongs to another entity, is 404, and without a reason it is 422; none leaves a request.
func TestARequestForAReversalIsValidated(t *testing.T) {
	w, other := newReversalWorld(t), newReversalWorld(t)

	unknown := postTo(t, w.a, "/v1/transactions/"+ids.New().String()+"/reversal", ids.New().String(), reversalBody)
	foreign := postTo(t, w.a, "/v1/transactions/"+other.posting+"/reversal", ids.New().String(), reversalBody)
	noReason := w.requestReversal(t, `{"reason": "  "}`)

	expectProblem(t, "unknown transaction", unknown, http.StatusNotFound, "not_found")
	expectProblem(t, "another entity's transaction", foreign, http.StatusNotFound, "not_found")
	expectProblem(t, "no reason", noReason, http.StatusUnprocessableEntity, "invalid_posting")
	var list struct{ Items []testApproval }
	if status := getJSON(t, w.a, "/v1/approvals", &list); status != http.StatusOK || len(list.Items) != 0 {
		t.Fatalf("requests = %d %+v, want none", status, list.Items)
	}
}
