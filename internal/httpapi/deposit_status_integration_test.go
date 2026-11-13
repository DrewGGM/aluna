//go:build integration

package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/party"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

func statusBody(status string) string { return `{"status":"` + status + `"}` }

// changeStatus does POST /v1/deposit-accounts/{id}/status-changes to status with a new Idempotency-Key.
func (w depositWorld) changeStatus(t *testing.T, id, status string) response {
	t.Helper()
	return w.move(t, id, "status-changes", statusBody(status))
}

// changed decodes the account of r, which must be 200.
func changed(t *testing.T, r response) gen.DepositAccount {
	t.Helper()
	var a gen.DepositAccount
	if err := json.Unmarshal(r.body, &a); err != nil || r.status != http.StatusOK {
		t.Fatalf("status change = %d %s (%v), want 200", r.status, r.body, err)
	}
	return a
}

// statusEvents are the types of the events about account a after its opening.
func statusEvents(t *testing.T, a gen.DepositAccount) []string {
	t.Helper()
	var types []string
	for _, e := range pgtest.EventsAbout(t, testPool, "deposit-accounts/"+a.Id.String())[1:] {
		types = append(types, e.Type)
	}
	return types
}

// TestAStatusChangeIsChecked (FR-22): a change the life cycle does not allow is a problem and changes nothing.
func TestAStatusChangeIsChecked(t *testing.T) {
	w := newDepositWorld(t)
	approved, savings := w.party(t, party.KYCApproved), w.savings(t, true)
	funded, held, frozen := w.opened(t, approved, savings), w.opened(t, approved, savings), w.opened(t, approved, savings)
	pending := w.opened(t, w.party(t, party.KYCPending), w.product(t, pendingKYCDefinition(t), true))
	expectCreated(t, "deposit", w.move(t, funded.Id.String(), "deposits", amountBody(1000)))
	expectCreated(t, "deposit", w.move(t, held.Id.String(), "deposits", amountBody(1000)))
	placeHoldOver(t, w.ts, held.LedgerAccountId.String(), createAccount(t, w.ts, "liability"), 500)
	w.do(t, func(tx postgres.Tx) error {
		return w.deposits.ChangeStatus(t.Context(), tx, w.entity, ids.ID(frozen.Id), deposits.Frozen, "operator-a")
	})

	for name, c := range map[string]struct {
		got    response
		status int
		code   string
	}{
		"close with a balance":          {w.changeStatus(t, funded.Id.String(), "closed"), http.StatusConflict, "account_not_empty"},
		"close with a pending hold":     {w.changeStatus(t, held.Id.String(), "closed"), http.StatusConflict, "pending_holds"},
		"freeze a frozen account":       {w.changeStatus(t, frozen.Id.String(), "frozen"), http.StatusConflict, "invalid_status_transition"},
		"close a frozen account":        {w.changeStatus(t, frozen.Id.String(), "closed"), http.StatusConflict, "invalid_status_transition"},
		"activate with the KYC pending": {w.changeStatus(t, pending.Id.String(), "active"), http.StatusUnprocessableEntity, "kyc_not_satisfied"},
		"set dormant by hand":           {w.changeStatus(t, funded.Id.String(), "dormant"), http.StatusBadRequest, "invalid_request"},
		"set pending by hand":           {w.changeStatus(t, funded.Id.String(), "pending"), http.StatusBadRequest, "invalid_request"},
		"an account of nowhere":         {w.changeStatus(t, ids.New().String(), "frozen"), http.StatusNotFound, "not_found"},
	} {
		if c.got.status != c.status || problemCode(t, c.got) != c.code {
			t.Errorf("%s = %d %s, want %d %s", name, c.got.status, c.got.body, c.status, c.code)
		}
	}
	for _, a := range []gen.DepositAccount{funded, held} {
		if events := statusEvents(t, a); len(events) != 0 {
			t.Errorf("account %s emitted %v after its rejected changes, want nothing", a.Id, events)
		}
	}
}

// pendingKYCDefinition is the savings product allowing a pending KYC, as a simplified deposit does: its accounts open pending.
func pendingKYCDefinition(t *testing.T) []byte {
	t.Helper()
	var definition map[string]json.RawMessage
	if err := json.Unmarshal(savingsDefinition(t), &definition); err != nil {
		t.Fatal(err)
	}
	definition["allowed_kyc"] = json.RawMessage(`["pending","approved"]`)
	document, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

// TestAnotherEntitysAccountDoesNotChange: an account of another entity is 404 and keeps its status.
func TestAnotherEntitysAccountDoesNotChange(t *testing.T) {
	w := newDepositWorld(t)
	a := w.opened(t, w.party(t, party.KYCApproved), w.savings(t, true))

	r := postTo(t, apiServer(t, ids.New()), "/v1/deposit-accounts/"+a.Id.String()+"/status-changes", ids.New().String(), statusBody("frozen"))

	expectProblem(t, "freeze another entity's account", r, http.StatusNotFound, "not_found")
	if events := statusEvents(t, a); len(events) != 0 {
		t.Fatalf("events = %v, want none", events)
	}
}

// TestOnlyAnOperatorChangesAStatus (ADR-0014): a channel, an integrator, compliance or an auditor cannot change an
// account's status.
func TestOnlyAnOperatorChangesAStatus(t *testing.T) {
	w := newDepositWorld(t)
	a := w.opened(t, w.party(t, party.KYCApproved), w.savings(t, true))
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}

	for _, role := range []string{"channel", "integrator", "compliance", "auditor"} {
		ts := apiServerAsRole(t, w.entity, events, "someone-"+role, role)
		r := postTo(t, ts, "/v1/deposit-accounts/"+a.Id.String()+"/status-changes", ids.New().String(), statusBody("frozen"))
		if r.status != http.StatusForbidden || problemCode(t, r) != "forbidden" {
			t.Errorf("as %s = %d %s, want 403 forbidden", role, r.status, r.body)
		}
	}
	if events := statusEvents(t, a); len(events) != 0 {
		t.Fatalf("events = %v, want none", events)
	}
}

// operatorB is the API as another operator of the world's entity, who decides what the world's operator requests.
func (w depositWorld) operatorB(t *testing.T) *httptest.Server {
	t.Helper()
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	return apiServerAs(t, w.entity, events, "operator-b")
}

// requestedChange asks for account a to go to status and returns the request, which must be pending (202).
func (w depositWorld) requestedChange(t *testing.T, a gen.DepositAccount, status string) testApproval {
	t.Helper()
	r := w.changeStatus(t, a.Id.String(), status)
	got := decodeApproval(t, r)
	if r.status != http.StatusAccepted || got.Status != "pending" || got.Kind != "deposits.account_status_change" {
		t.Fatalf("change to %s = %d %s, want 202 with a pending deposits.account_status_change request", status, r.status, r.body)
	}
	var payload map[string]any
	if err := json.Unmarshal(got.Payload, &payload); err != nil || payload["account_id"] != a.Id.String() || payload["status"] != status {
		t.Fatalf("payload = %s (%v), want the account and %s", got.Payload, err, status)
	}
	return got
}

// approve approves request id as ts and returns the request as it ends.
func approve(t *testing.T, ts *httptest.Server, id string) testApproval {
	t.Helper()
	r := postTo(t, ts, "/v1/approvals/"+id+"/approval", ids.New().String(), "")
	if r.status != http.StatusOK {
		t.Fatalf("approve %s = %d %s, want 200", id, r.status, r.body)
	}
	return decodeApproval(t, r)
}

// readAccount reads account a, which must be 200.
func (w depositWorld) readAccount(t *testing.T, a gen.DepositAccount) gen.DepositAccount {
	t.Helper()
	var read gen.DepositAccount
	if status := call(t, w.ts, http.MethodGet, "/v1/deposit-accounts/"+a.Id.String(), "", &read); status != http.StatusOK {
		t.Fatalf("read = %d", status)
	}
	return read
}

// TestAFreezeNeedsAnotherOperator (FR-22, FR-39): a freeze is a request that changes nothing until another operator
// approves it, not its maker; then the account is frozen, by the approver, with its event. Unfreezing goes the same
// way and restarts the customer's inactivity.
func TestAFreezeNeedsAnotherOperator(t *testing.T) {
	w := newDepositWorld(t)
	a, b := w.opened(t, w.party(t, party.KYCApproved), w.savings(t, true)), w.operatorB(t)

	freeze := w.requestedChange(t, a, "frozen")
	if w.readAccount(t, a).Status != gen.DepositAccountStatusActive || len(statusEvents(t, a)) != 0 {
		t.Fatalf("before the approval the account is %s with events %v, want it active and unchanged", w.readAccount(t, a).Status, statusEvents(t, a))
	}
	expectProblem(t, "the maker approving", postTo(t, w.ts, "/v1/approvals/"+freeze.ID+"/approval", ids.New().String(), ""),
		http.StatusForbidden, "self_approval_forbidden")
	if got := approve(t, b, freeze.ID); got.Status != "approved" || w.readAccount(t, a).Status != gen.DepositAccountStatusFrozen {
		t.Fatalf("approved = %+v and the account is %s, want it approved and frozen", got, w.readAccount(t, a).Status)
	}
	if got := approve(t, b, w.requestedChange(t, a, "active").ID); got.Status != "approved" {
		t.Fatalf("approve the unfreeze = %+v, want approved", got)
	}

	if read := w.readAccount(t, a); read.Status != gen.DepositAccountStatusActive || read.LastCustomerActivityOn.String() != "2026-03-02" {
		t.Fatalf("after the unfreeze = %+v, want active with activity on 2026-03-02", read)
	}
	if got, want := statusEvents(t, a), []string{"aluna.deposits.account.frozen.v1", "aluna.deposits.account.unfrozen.v1"}; !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if byB := changesBy(t, a, "operator-b"); byB != 2 {
		t.Fatalf("changes recorded by operator-b = %d, want 2", byB)
	}
}

// changesBy is how many status changes of account a actor made.
func changesBy(t *testing.T, a gen.DepositAccount, actor string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM deposits.status_changes WHERE account_id = $1 AND actor = $2`,
		a.Id, actor).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestActivationAndClosureAreDirect (FR-22): activating a pending account whose KYC was approved and closing an empty
// account need no approval: 200 with the account, and their events.
func TestActivationAndClosureAreDirect(t *testing.T) {
	w := newDepositWorld(t)
	customer := w.party(t, party.KYCPending)
	a := w.opened(t, customer, w.product(t, pendingKYCDefinition(t), true))
	partyID, err := ids.Parse(customer)
	if err != nil {
		t.Fatal(err)
	}
	w.do(t, func(tx postgres.Tx) error {
		_, err := w.parties.ChangeKYC(t.Context(), tx, w.entity, partyID, 1, party.KYCApproved)
		return err
	})

	activated := changed(t, w.changeStatus(t, a.Id.String(), "active"))
	closed := changed(t, w.changeStatus(t, a.Id.String(), "closed"))

	if a.Status != gen.DepositAccountStatusPending || activated.Status != gen.DepositAccountStatusActive || closed.Status != gen.DepositAccountStatusClosed {
		t.Fatalf("opened %s, activated %s, closed %s; want pending, active and closed", a.Status, activated.Status, closed.Status)
	}
	if got, want := statusEvents(t, a), []string{"aluna.deposits.account.activated.v1", "aluna.deposits.account.closed.v1"}; !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

// TestADormantAccountIsReactivatedAtOnce (FR-22): reactivating a dormant account needs no approval: 200, active,
// with its event.
func TestADormantAccountIsReactivatedAtOnce(t *testing.T) {
	w := newDepositWorld(t)
	a := w.opened(t, w.party(t, party.KYCApproved), w.savings(t, true))
	w.do(t, func(tx postgres.Tx) error {
		return w.deposits.ChangeStatus(t.Context(), tx, w.entity, ids.ID(a.Id), deposits.Dormant, "operator-a")
	})

	reactivated := changed(t, w.changeStatus(t, a.Id.String(), "active"))

	if reactivated.Status != gen.DepositAccountStatusActive {
		t.Fatalf("reactivated = %s, want active", reactivated.Status)
	}
	if got, want := statusEvents(t, a), []string{"aluna.deposits.account.dormant.v1", "aluna.deposits.account.activated.v1"}; !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

// TestComplianceCannotApproveAFreeze (ADR-0014): compliance decides requests but cannot change an account's status,
// so its approval of a freeze is 403 and the account stays active.
func TestComplianceCannotApproveAFreeze(t *testing.T) {
	w := newDepositWorld(t)
	a := w.opened(t, w.party(t, party.KYCApproved), w.savings(t, true))
	freeze := w.requestedChange(t, a, "frozen")
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	compliance := apiServerAsRole(t, w.entity, events, "someone-compliance", "compliance")

	decided := postTo(t, compliance, "/v1/approvals/"+freeze.ID+"/approval", ids.New().String(), "")

	expectProblem(t, "compliance approves a freeze", decided, http.StatusForbidden, "forbidden")
	if w.readAccount(t, a).Status != gen.DepositAccountStatusActive {
		t.Fatalf("the account is %s, want active", w.readAccount(t, a).Status)
	}
}

// TestAFreezeOfAnAccountClosedMeanwhileFails (FR-39): a freeze approved after the account was closed leaves the
// request failed and the account closed.
func TestAFreezeOfAnAccountClosedMeanwhileFails(t *testing.T) {
	w := newDepositWorld(t)
	a, b := w.opened(t, w.party(t, party.KYCApproved), w.savings(t, true)), w.operatorB(t)
	freeze := w.requestedChange(t, a, "frozen")
	changed(t, w.changeStatus(t, a.Id.String(), "closed"))

	got := approve(t, b, freeze.ID)

	if got.Status != "failed" || w.readAccount(t, a).Status != gen.DepositAccountStatusClosed {
		t.Fatalf("approve = %+v with the account %s, want failed and closed", got, w.readAccount(t, a).Status)
	}
	if events := statusEvents(t, a); !slices.Equal(events, []string{"aluna.deposits.account.closed.v1"}) {
		t.Fatalf("events = %v, want only the closure", events)
	}
}

// TestAnOldUnfreezeDoesNotLiftANewFreeze (FR-39): an unfreeze requested before the account was unfrozen and frozen
// again fails when approved, and the new freeze stays.
func TestAnOldUnfreezeDoesNotLiftANewFreeze(t *testing.T) {
	w := newDepositWorld(t)
	a, b := w.opened(t, w.party(t, party.KYCApproved), w.savings(t, true)), w.operatorB(t)
	approve(t, b, w.requestedChange(t, a, "frozen").ID)
	stale := w.requestedChange(t, a, "active")
	approve(t, b, w.requestedChange(t, a, "active").ID)
	approve(t, b, w.requestedChange(t, a, "frozen").ID)

	got := approve(t, b, stale.ID)

	if got.Status != "failed" || w.readAccount(t, a).Status != gen.DepositAccountStatusFrozen {
		t.Fatalf("approve the old unfreeze = %+v with the account %s, want failed and frozen", got, w.readAccount(t, a).Status)
	}
	want := []string{"aluna.deposits.account.frozen.v1", "aluna.deposits.account.unfrozen.v1", "aluna.deposits.account.frozen.v1"}
	if events := statusEvents(t, a); !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

// TestAStatusChangeIsReplayed: the same Idempotency-Key answers the same 202 and requests the freeze once.
func TestAStatusChangeIsReplayed(t *testing.T) {
	w := newDepositWorld(t)
	a := w.opened(t, w.party(t, party.KYCApproved), w.savings(t, true))
	path, key := "/v1/deposit-accounts/"+a.Id.String()+"/status-changes", ids.New().String()

	first, again := postTo(t, w.ts, path, key, statusBody("frozen")), postTo(t, w.ts, path, key, statusBody("frozen"))

	if first.status != http.StatusAccepted || again.status != http.StatusAccepted || !bytes.Equal(again.body, first.body) {
		t.Fatalf("freeze = %d %s and again %d %s, want the same 202", first.status, first.body, again.status, again.body)
	}
	var requests int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM approvals.requests WHERE payload->>'account_id' = $1`,
		a.Id.String()).Scan(&requests); err != nil || requests != 1 {
		t.Fatalf("requests about the account = %d (%v), want 1", requests, err)
	}
}
