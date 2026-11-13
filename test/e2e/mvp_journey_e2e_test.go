//go:build e2e

package e2e

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/coreos/go-oidc/v3/oidc/oidctest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DrewGGM/aluna/internal/accounting"
	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/compliance"
	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/party"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
	"github.com/DrewGGM/aluna/internal/pricing"
	"github.com/DrewGGM/aluna/internal/products"
	"github.com/DrewGGM/aluna/internal/reconciliation"
	"github.com/DrewGGM/aluna/internal/webhooks"
)

// The journey's business days: the entity starts on Friday 2026-10-30, October's last business day, and its
// rollovers take it to Tuesday 2026-11-03 (Monday the 2nd is All Saints' Day) and then to Wednesday the 4th.
var (
	october30 = calendar.Date{Year: 2026, Month: time.October, Day: 30}
	november3 = calendar.Date{Year: 2026, Month: time.November, Day: 3}
)

// journeyRailSecret is what the simulated rail signs with; api and worker share it.
const journeyRailSecret = "a secret of the simulated rail, for the MVP journey"

// transferFee is the savings product's fee for a transfer or a payment, in units: 3,000 COP.
const transferFee = 300_000

// gmf is the 4 per thousand that a movement of units pays.
func gmf(units int64) int64 { return units * 4 / 1000 }

// savingsDefinition capitalizes monthly with modified_following, so October's capitalization falls on Friday the
// 30th: Saturday the 31st moved forward would land in November. Its fee is transferFee.
const savingsDefinition = `{
	"currency": "COP", "day_count": "ACT/365F", "rate_tiers": [{"from_units": 0, "rate_ea": "0.12"}],
	"capitalization_frequency": "monthly", "date_convention": "modified_following", "dormancy_days": 365,
	"fees": [{"code": "transfer_fee", "trigger": "transfer", "amount_units": 300000}],
	"allowed_kyc": ["approved"], "gmf_exempt_eligible": true,
	"accounts": {"cash": "A.CASH.VAULT", "interest_expense": "E.INTEREST.SAVINGS", "fee_income": "I.FEES.SAVINGS", "gmf_payable": "L.TAX.GMF"}
}`

// cuifMappings cover every ledger account the journey moves, from its first business day.
const cuifMappings = `pattern,cuif_code,valid_from,valid_to
deposits/,210505,2026-10-30,2028-01-01
A.CASH.,110505,2026-10-30,2028-01-01
L.CLEARING.,259500,2026-10-30,2028-01-01
L.SUSPENSE.,259505,2026-10-30,2028-01-01
L.TAX.GMF,244500,2026-10-30,2028-01-01
I.FEES.,416500,2026-10-30,2028-01-01
E.INTEREST.,510500,2026-10-30,2028-01-01
`

// TestMVPJourney (US-1 to US-16): with the real binary, a product published under maker-checker, a customer with two
// accounts, a deposit, a quoted transfer, a payment out through the simulated rail and one back, October's interest
// capitalized after the rollover, its daily GL, its close and its statement, and an integrator's webhook that hears of
// it; the continuous controls end without breaks. Corrupting a customer's balance and the rail's afterwards opens
// their breaks, so controls that looked at nothing would fail here.
func TestMVPJourney(t *testing.T) {
	j := newJourney(t)
	j.loadCUIFMappings(t)
	j.createLedgerAccounts(t)
	product := j.publishProduct(t)
	customer := j.registerCustomer(t)
	a, b := j.openAccount(t, customer, product), j.openAccount(t, customer, product)
	j.deposit(t, a, 1_000_000_000)
	j.quotedTransfer(t, a, b, 100_000_000)
	j.payThroughTheRail(t, a, 30_000_000)
	// once the worker has stopped: its delivery job would claim the endpoint's deliveries and fail them for the
	// receiver's loopback address
	hooks := j.followWebhooks(t)
	// as much as the payment, so the clearing account ends October at zero: nothing settles it against the rail yet
	// (Q-127 b)
	j.receiveFromTheRail(t, a, 30_000_000)
	october := j.balance(t, a.LedgerAccountID)

	j.rollover(t, october30)
	capitalized := j.capitalize(t, a, october)
	j.expectGL(t, "2026-10-30", 30_000_000)
	j.expectNoBreaks(t)
	j.closePeriod(t, "2026-10")
	j.expectStatement(t, a, "2026-10", october, 2*transferFee, gmf(100_000_000)+gmf(30_000_000))
	j.expectWebhook(t, hooks, a, capitalized)
	j.expectNoBreaks(t)

	j.corruptAndExpectBreaks(t, a)
}

// journey is the running api of a new entity, the database it shares with the jobs the test runs in its own
// process, and who signs the tokens of the entity's people.
type journey struct {
	bin, appDSN, apiURL, railAddr string
	entity                        ids.ID
	sign                          func(subject, role string) string
	pool                          *pgxpool.Pool
	uow                           *postgres.UnitOfWork
	jobs                          jobs
	accounts                      map[string]string // ledger account ids by code
}

// newJourney builds aluna, migrates a fresh database with it, creates the entity on October 30 with aluna entity create
// and starts aluna api.
func newJourney(t *testing.T) *journey {
	t.Helper()
	bin := buildAluna(t)
	dsn, cleanup, err := pgtest.StartEmpty(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	if out, err := alunaCmd(t, bin, []string{"ALUNA_DATABASE_URL=" + dsn}, "migrate", "up").CombinedOutput(); err != nil {
		t.Fatalf("aluna migrate up: %v\n%s", err, out)
	}
	appDSN, err := pgtest.AppDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := postgres.Open(t.Context(), appDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	j := &journey{bin: bin, appDSN: appDSN, entity: createEntity(t, bin, dsn), pool: pool, uow: postgres.NewUnitOfWork(pool), jobs: newJobs(t, pool)}
	issuer, sign := rolesIssuer(t, j.entity)
	j.sign = sign
	addr := freeAddr(t)
	j.railAddr, j.apiURL = freeAddr(t), "http://"+addr
	start(t, bin, "api", "ALUNA_HTTP_ADDR="+addr, "ALUNA_RAIL_ADDR="+j.railAddr, "ALUNA_DATABASE_URL="+appDSN,
		"ALUNA_OIDC_ISSUER="+issuer, "ALUNA_OIDC_AUDIENCE="+audience, "ALUNA_SIMBREB_SECRET="+journeyRailSecret)
	waitReady(t, j.apiURL+"/readyz")
	return j
}

// createEntity registers, with aluna entity create as the tables' owner of dsn, the journey's entity on October 30.
func createEntity(t *testing.T, bin, dsn string) ids.ID {
	t.Helper()
	cmd := alunaCmd(t, bin, []string{"ALUNA_DATABASE_URL=" + dsn}, "entity", "create", "--name", "Entity", "--business-date", october30.String(), "--currency", "COP")
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("aluna entity create: %v\n%s", err, errOut.String())
	}
	entity, err := ids.Parse(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("aluna entity create printed %q: %v", out, err)
	}
	return entity
}

// jobs are the time-driven jobs of the worker, run by the test when the journey needs them: the worker runs them on
// intervals of minutes to an hour.
type jobs struct {
	deposits       *deposits.Service
	accounting     *accounting.Service
	reconciliation *reconciliation.Service
	webhooks       *webhooks.Service
}

// newJobs wires the services the way aluna worker does.
func newJobs(t *testing.T, pool *pgxpool.Pool) jobs {
	t.Helper()
	events, err := outbox.New(pool)
	if err != nil {
		t.Fatal(err)
	}
	calendarSvc, productSvc, complianceSvc := calendar.New(events, time.Now), products.New(events), compliance.New(events)
	pricingSvc := pricing.New(productSvc, calendarSvc, pricing.SingleAccount)
	depositSvc := deposits.New(ledger.New(calendarSvc, events), party.New(events, complianceSvc.PartyScreener()), productSvc, calendarSvc,
		pricingSvc, events)
	return jobs{
		deposits: depositSvc, accounting: accounting.New(calendarSvc, events), reconciliation: reconciliation.New(events), webhooks: webhooks.New(),
	}
}

// rolesIssuer is a test OIDC issuer and a function that signs the token of subject with role, all of entity.
func rolesIssuer(t *testing.T, entity ids.ID) (issuer string, sign func(subject, role string) string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ts := serveIssuer(t, &oidctest.Server{PublicKeys: []oidctest.PublicKey{{PublicKey: key.Public(), KeyID: "k1", Algorithm: oidc.RS256}}})
	return ts.URL, func(subject, role string) string {
		now := time.Now()
		claims, err := json.Marshal(map[string]any{
			"iss": ts.URL, "aud": audience, "sub": subject, "iat": now.Unix(), "exp": now.Add(15 * time.Minute).Unix(),
			"legal_entity_id": entity.String(), "roles": []string{role},
		})
		if err != nil {
			t.Fatal(err)
		}
		return oidctest.SignIDToken(key, "k1", oidc.RS256, string(claims))
	}
}

// start runs aluna command with env and returns what kills it and waits for it to exit. It also dies with the test,
// and if the test fails its log is printed.
func start(t *testing.T, bin, command string, env ...string) (stop func()) {
	t.Helper()
	cmd := alunaCmd(t, bin, env, command)
	var logs bytes.Buffer // os/exec does not write to Stdout and Stderr concurrently if they are the same writer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	wait := sync.OnceValue(cmd.Wait)
	t.Cleanup(func() {
		wait() //nolint:errcheck,gosec // the process dies with the test context
		if t.Failed() {
			t.Logf("aluna %s log:\n%s", command, logs.String())
		}
	})
	return func() {
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		wait() //nolint:errcheck,gosec // killed on purpose
	}
}

// as is the api with the session of subject, who has role.
func (j *journey) as(subject, role string) runningAPI {
	return runningAPI{base: j.apiURL, token: j.sign(subject, role)}
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

// loadCUIFMappings loads cuifMappings with finance's maker-checker.
func (j *journey) loadCUIFMappings(t *testing.T) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"csv": cuifMappings})
	if err != nil {
		t.Fatal(err)
	}
	result := makerChecker(t, j.as("finance-a", "finance"), j.as("finance-b", "finance"), "/v1/cuif-mappings", string(body))
	var loaded struct{ Mappings int }
	if err := json.Unmarshal(result, &loaded); err != nil || loaded.Mappings != 7 {
		t.Fatalf("CUIF mappings loaded: %s (%v), want 7", result, err)
	}
}

// createLedgerAccounts creates the accounts the product and the rail name, all sync so their balances are exact.
func (j *journey) createLedgerAccounts(t *testing.T) {
	t.Helper()
	j.accounts = map[string]string{}
	for code, kind := range map[string]string{
		"A.CASH.VAULT": "asset", "E.INTEREST.SAVINGS": "expense", "I.FEES.SAVINGS": "income", "L.TAX.GMF": "liability",
		"L.CLEARING.SIMBREB": "liability", "L.SUSPENSE.SIMBREB": "liability",
	} {
		var account struct{ ID string }
		body := fmt.Sprintf(`{"code":%q,"kind":%q,"currency":"COP","balance_mode":"sync"}`, code, kind)
		expect(t, j.as("operator-a", "operator"), http.MethodPost, "/v1/ledger-accounts", body, http.StatusCreated, &account)
		j.accounts[code] = account.ID
	}
}

// publishProduct creates the savings product and publishes its first version: an operator asks, finance approves.
func (j *journey) publishProduct(t *testing.T) string {
	t.Helper()
	operator := j.as("operator-a", "operator")
	var product, version struct{ ID, Status string }
	expect(t, operator, http.MethodPost, "/v1/products", `{"code":"savings","name":"Savings account"}`, http.StatusCreated, &product)
	expect(t, operator, http.MethodPost, "/v1/products/"+product.ID+"/versions", `{"definition":`+savingsDefinition+`}`,
		http.StatusCreated, &version)
	makerChecker(t, operator, j.as("finance-a", "finance"), "/v1/product-versions/"+version.ID+"/publication", "")
	expect(t, operator, http.MethodGet, "/v1/product-versions/"+version.ID, "", http.StatusOK, &version)
	if version.Status != "published" {
		t.Fatalf("version %s is %s, want published", version.ID, version.Status)
	}
	return product.ID
}

// registerCustomer registers a natural person and approves their KYC: an operator asks, compliance approves.
func (j *journey) registerCustomer(t *testing.T) string {
	t.Helper()
	operator := j.as("operator-a", "operator")
	var p struct{ ID string }
	body := `{"kind":"natural","external_ref":"onb-` + ids.New().String() + `","risk_level":"low","pep":false,"consents":{"personal_data":true}}`
	expect(t, operator, http.MethodPost, "/v1/parties", body, http.StatusCreated, &p)
	makerChecker(t, operator, j.as("compliance-a", "compliance"), "/v1/parties/"+p.ID+"/kyc-status-changes", `{"kyc_status":"approved"}`)
	return p.ID
}

// depositAccount is what the journey reads of a deposit account.
type depositAccount struct {
	ID              string `json:"id"`
	LedgerAccountID string `json:"ledger_account_id"`
	Status          string `json:"status"`
}

func (j *journey) openAccount(t *testing.T, partyID, productID string) depositAccount {
	t.Helper()
	var a depositAccount
	expect(t, j.as("operator-a", "operator"), http.MethodPost, "/v1/deposit-accounts",
		`{"party_id":"`+partyID+`","product_id":"`+productID+`"}`, http.StatusCreated, &a)
	if a.Status != "active" {
		t.Fatalf("account %s opened %s, want active", a.ID, a.Status)
	}
	return a
}

// balance is the natural balance of a ledger account: a customer's deposit is positive.
func (j *journey) balance(t *testing.T, ledgerAccount string) int64 {
	t.Helper()
	var b struct{ Natural struct{ Amount int64 } }
	expect(t, j.as("operator-a", "operator"), http.MethodGet, "/v1/ledger-accounts/"+ledgerAccount+"/balance", "", http.StatusOK, &b)
	return b.Natural.Amount
}

func cop(units int64) string {
	return `{"amount":` + strconv.FormatInt(units, 10) + `,"currency":"COP"}`
}

// deposit deposits units of cash into a.
func (j *journey) deposit(t *testing.T, a depositAccount, units int64) {
	t.Helper()
	expect(t, j.as("operator-a", "operator"), http.MethodPost, "/v1/deposit-accounts/"+a.ID+"/deposits", `{"amount":`+cop(units)+`}`,
		http.StatusCreated, nil)
	if got := j.balance(t, a.LedgerAccountID); got != units {
		t.Fatalf("balance after the deposit = %d, want %d", got, units)
	}
}

// quotedTransfer quotes a transfer of units from a to b and makes it with the quote: a pays exactly what it showed,
// the product's fee and 4 per thousand of GMF.
func (j *journey) quotedTransfer(t *testing.T, a, b depositAccount, units int64) {
	t.Helper()
	operator := j.as("operator-a", "operator")
	var q struct {
		ID        string
		TotalFees struct{ Amount int64 } `json:"total_fees"`
		GMF       struct{ Amount int64 } `json:"gmf"`
	}
	expect(t, operator, http.MethodPost, "/v1/quotes", `{"account_id":"`+a.ID+`","operation":"transfer","amount":`+cop(units)+`}`,
		http.StatusCreated, &q)
	if q.TotalFees.Amount != transferFee || q.GMF.Amount != gmf(units) {
		t.Fatalf("quote = %+v, want the 3,000 COP fee and 4 per thousand of GMF", q)
	}
	fromBefore, toBefore := j.balance(t, a.LedgerAccountID), j.balance(t, b.LedgerAccountID)
	body := `{"from_account_id":"` + a.ID + `","to_account_id":"` + b.ID + `","amount":` + cop(units) + `,"quote_id":"` + q.ID + `"}`
	expect(t, operator, http.MethodPost, "/v1/transfers", body, http.StatusCreated, nil)
	if from, to := j.balance(t, a.LedgerAccountID), j.balance(t, b.LedgerAccountID); from != fromBefore-units-q.TotalFees.Amount-q.GMF.Amount ||
		to != toBefore+units {
		t.Fatalf("balances after the transfer %d and %d, from %d and %d with the quote %+v", from, to, fromBefore, toBefore, q)
	}
}

// payThroughTheRail pays units from a to a key through the simulated rail with aluna worker running: the worker sends
// the order and the rail notifies the api, which settles it. The worker stops afterwards, so the jobs of the rest of
// the journey run only when the test runs them.
func (j *journey) payThroughTheRail(t *testing.T, a depositAccount, units int64) {
	t.Helper()
	stop := start(t, j.bin, "worker", "ALUNA_DATABASE_URL="+j.appDSN, "ALUNA_AUDIT_ANCHOR_DIR="+t.TempDir(),
		"ALUNA_SIMBREB_SECRET="+journeyRailSecret, "ALUNA_SIMBREB_NOTIFY_URL=http://"+j.railAddr)
	defer stop()
	before := j.balance(t, a.LedgerAccountID)
	var order struct {
		Status string
		Held   struct{ Amount int64 }
	}
	body := `{"account_id":"` + a.ID + `","amount":` + cop(units) + `,"rail":"simbreb","creditor":{"key":"@beneficiary"}}`
	expect(t, j.as("operator-a", "operator"), http.MethodPost, "/v1/payment-orders", body, http.StatusAccepted, &order)
	if order.Status != "held" || order.Held.Amount != units+transferFee+gmf(units) {
		t.Fatalf("order = %+v, want it held with its fee and its GMF", order)
	}
	eventually(t, "the payment's settlement", func() bool { return j.balance(t, a.LedgerAccountID) == before-order.Held.Amount })
}

// webhookSignature is the Standard Webhooks signature of a message: v1, and the base64 HMAC-SHA256 with key of
// id.timestamp.body.
func webhookSignature(key []byte, id, timestamp, body string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + timestamp + "." + body))
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// eventually checks cond every 100 ms until it holds, for up to a minute.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(time.Minute); !cond(); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within a minute", what)
		}
	}
}

// receiveFromTheRail has the simulated rail deliver to the api a transfer of units another bank sent to a, signed as
// the rail signs it (Standard Webhooks), on October 30.
func (j *journey) receiveFromTheRail(t *testing.T, a depositAccount, units int64) {
	t.Helper()
	before := j.balance(t, a.LedgerAccountID)
	ref := strconv.FormatInt(time.Now().UnixNano(), 10)
	body := fmt.Sprintf(`{"creditor_agent":%q,"end_to_end_id":"BANKX%s","instr_id":"INSTR1","tx_id":"TX1","rail_ref":"SIMIN%s",`+
		`"debtor_agent":"BANKXCOBB","amount":%d,"currency":"COP","settlement_date":"2026-10-30","creditor":{"account_number":%q}}`,
		j.entity, ref, ref, units, a.ID)
	id, timestamp := "SIMIN"+ref, strconv.FormatInt(time.Now().Unix(), 10)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+j.railAddr+"/v1/rails/simbreb/transfers", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("webhook-id", id)
	req.Header.Set("webhook-timestamp", timestamp)
	req.Header.Set("webhook-signature", webhookSignature([]byte(journeyRailSecret), id, timestamp, body))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close() //nolint:errcheck,gosec // close in a test
	if resp.StatusCode != http.StatusNoContent || j.balance(t, a.LedgerAccountID) != before+units {
		t.Fatalf("the transfer received = %d, balance %d from %d, want 204 and %d more", resp.StatusCode, j.balance(t, a.LedgerAccountID), before, units)
	}
}

// rollover has the operator's scheduler, a system client, close today and advance the entity's business date to the
// next business day, as it does at the end of each business day.
func (j *journey) rollover(t *testing.T, today calendar.Date) {
	t.Helper()
	var rolled struct {
		Closed string `json:"closed_business_date"`
	}
	expect(t, j.as("scheduler", "system"), http.MethodPost, "/v1/business-date/rollover", `{"business_date":"`+today.String()+`"}`, http.StatusOK, &rolled)
	if rolled.Closed != today.String() {
		t.Fatalf("the rollover closed %s, want %s", rolled.Closed, today)
	}
}

// capitalize runs the accrual of the closed days, which capitalizes October's interest into a on October 30, and
// returns how much it capitalized.
func (j *journey) capitalize(t *testing.T, a depositAccount, before int64) int64 {
	t.Helper()
	accrued, err := j.jobs.deposits.Accrue(t.Context(), j.uow, 50)
	if err != nil {
		t.Fatal(err)
	}
	capitalized := j.balance(t, a.LedgerAccountID) - before
	// two accounts from October 30 to November 2: four days each
	if accrued != 8 || capitalized <= 0 {
		t.Fatalf("accrued %d account-days and capitalized %d units, want 8 and October's interest", accrued, capitalized)
	}
	return capitalized
}

// expectGL projects the daily GL and checks, over HTTP, that day squares and that the rail's clearing account moved
// railUnits each way: the payment out and the transfer back.
func (j *journey) expectGL(t *testing.T, day string, railUnits int64) {
	t.Helper()
	if err := j.jobs.accounting.ProjectGL(t.Context(), j.uow); err != nil {
		t.Fatal(err)
	}
	var gl struct {
		Lines []struct {
			CUIFCode string `json:"cuif_code"`
			Debits   struct{ Amount int64 }
			Credits  struct{ Amount int64 }
		}
	}
	expect(t, j.as("finance-a", "finance"), http.MethodGet, "/v1/general-ledger/daily?date="+day, "", http.StatusOK, &gl)
	var debits, credits int64
	clearing := false
	for _, l := range gl.Lines {
		debits, credits = debits+l.Debits.Amount, credits+l.Credits.Amount
		clearing = clearing || l.CUIFCode == "259500" && l.Debits.Amount == railUnits && l.Credits.Amount == railUnits
	}
	if debits == 0 || debits != credits || !clearing {
		t.Fatalf("GL of %s = %+v, want it square with the clearing account moving %d each way", day, gl.Lines, railUnits)
	}
}

// openBreaks are the controls of the entity's open breaks, as finance reads them.
func (j *journey) openBreaks(t *testing.T) []string {
	t.Helper()
	var list struct{ Breaks []struct{ Control string } }
	expect(t, j.as("finance-a", "finance"), http.MethodGet, "/v1/reconciliation/breaks?status=open", "", http.StatusOK, &list)
	controls := make([]string, 0, len(list.Breaks))
	for _, b := range list.Breaks {
		controls = append(controls, b.Control)
	}
	return controls
}

// expectNoBreaks runs the continuous controls and checks they leave no break open.
func (j *journey) expectNoBreaks(t *testing.T) {
	t.Helper()
	if err := j.jobs.reconciliation.RunControls(t.Context(), j.uow); err != nil {
		t.Fatal(err)
	}
	if open := j.openBreaks(t); len(open) != 0 {
		t.Fatalf("open breaks of %v, want none", open)
	}
}

// closePeriod closes period with finance's maker-checker; afterwards nothing posts in it.
func (j *journey) closePeriod(t *testing.T, period string) {
	t.Helper()
	result := makerChecker(t, j.as("finance-a", "finance"), j.as("finance-b", "finance"), "/v1/accounting-periods/"+period+"/closure", "")
	var closed struct{ Period, Status string }
	if err := json.Unmarshal(result, &closed); err != nil || closed.Period != period || closed.Status != "closed" {
		t.Fatalf("close %s = %s (%v)", period, result, err)
	}
	var problem struct{ Code string }
	expect(t, j.as("operator-a", "operator"), http.MethodPost, "/v1/transactions", posting("2026-10-30", j.accounts["A.CASH.VAULT"],
		j.accounts["L.SUSPENSE.SIMBREB"]), http.StatusConflict, &problem)
	if problem.Code != "period_closed" {
		t.Fatalf("posting in %s = %s, want period_closed", period, problem.Code)
	}
}

// posting is the body of a manual posting of 1,000 COP from debit to credit on day.
func posting(day, debit, credit string) string {
	return fmt.Sprintf(`{"code":"adjustment","value_date":%q,"business_date":%q,"source":"e2e","external_ref":%q,"entries":[
		{"account_id":%q,"amount":{"amount":100000,"currency":"COP"}},{"account_id":%q,"amount":{"amount":-100000,"currency":"COP"}}]}`,
		day, day, ids.New(), debit, credit)
}

// expectStatement generates the statements of the closed periods and checks a's of period: from zero to its closing
// balance, the one the journey read at the end of October, with the fees and the GMF of the transfer and the payment.
func (j *journey) expectStatement(t *testing.T, a depositAccount, period string, closing, fees, gmfUnits int64) {
	t.Helper()
	if _, err := j.jobs.deposits.GenerateStatements(t.Context(), j.uow, 200); err != nil {
		t.Fatal(err)
	}
	var s struct {
		Document struct {
			OpeningBalance int64 `json:"opening_balance"`
			ClosingBalance int64 `json:"closing_balance"`
			Fees           int64 `json:"fees"`
			GMF            int64 `json:"gmf"`
			Movements      []struct{ Kind string }
		}
	}
	expect(t, j.as("operator-a", "operator"), http.MethodGet, "/v1/deposit-accounts/"+a.ID+"/statements/"+period, "", http.StatusOK, &s)
	if d := s.Document; d.OpeningBalance != 0 || d.ClosingBalance != closing || d.Fees != fees || d.GMF != gmfUnits || len(d.Movements) == 0 {
		t.Fatalf("statement of %s = %+v, want it from 0 to %d with %d of fees and %d of GMF", period, d, closing, fees, gmfUnits)
	}
}

// hookReceiver is an integrator's endpoint, on its own TLS server: it checks each delivery's signature by the
// Standard Webhooks rules, with the secret its registration returned, and keeps the events.
type hookReceiver struct {
	key    []byte
	server *httptest.Server

	mu     sync.Mutex
	events []hookEvent
	bad    []string
}

// hookEvent is what the journey reads of an event delivered.
type hookEvent struct {
	Type string
	Data struct {
		AccountID string `json:"account_id"`
		Amount    int64  `json:"amount"`
	}
}

func (r *hookReceiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	id, timestamp := req.Header.Get("webhook-id"), req.Header.Get("webhook-timestamp")
	var e hookEvent
	if err != nil || req.Header.Get("webhook-signature") != webhookSignature(r.key, id, timestamp, string(body)) ||
		json.Unmarshal(body, &e) != nil {
		r.bad = append(r.bad, id)
	} else {
		r.events = append(r.events, e)
	}
	w.WriteHeader(http.StatusNoContent)
}

// capitalized is how much the delivered capitalization event of account says, if one came.
func (r *hookReceiver) capitalized(account string) (int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.Type == "aluna.deposits.interest.capitalized.v1" && e.Data.AccountID == account {
			return e.Data.Amount, true
		}
	}
	return 0, false
}

// badDeliveries are the ids of the deliveries whose signature did not check.
func (r *hookReceiver) badDeliveries() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.bad)
}

// followWebhooks has an integrator register a public https endpoint, which then, as the tables' owner, is pointed at
// a local receiver: the registration and the production sender refuse loopback addresses.
func (j *journey) followWebhooks(t *testing.T) *hookReceiver {
	t.Helper()
	var endpoint struct{ ID, Secret string }
	expect(t, j.as("integrator-a", "integrator"), http.MethodPost, "/v1/webhook-endpoints", `{"url":"https://93.184.216.34/hooks"}`,
		http.StatusCreated, &endpoint)
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(endpoint.Secret, "whsec_"))
	if err != nil {
		t.Fatal(err)
	}
	r := &hookReceiver{key: key}
	r.server = httptest.NewTLSServer(r)
	t.Cleanup(r.server.Close)
	pgtest.AsOwner(t, j.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE webhooks.endpoints SET url = $1 WHERE id = $2`, r.server.URL+"/hooks", endpoint.ID)
		return err
	})
	return r
}

// expectWebhook delivers the webhooks until the endpoint has a's capitalization, which must say capitalized, every
// delivery signed.
func (j *journey) expectWebhook(t *testing.T, r *hookReceiver, a depositAccount, capitalized int64) {
	t.Helper()
	var got int64
	eventually(t, "the capitalization's webhook", func() bool {
		if err := j.jobs.webhooks.Deliver(t.Context(), j.uow, r.server.Client(), time.Now, 50); err != nil {
			t.Fatal(err)
		}
		var ok bool
		got, ok = r.capitalized(a.ID)
		return ok
	})
	if bad := r.badDeliveries(); got != capitalized || len(bad) != 0 {
		t.Fatalf("webhook of the capitalization says %d, want %d; deliveries with a bad signature: %v", got, capitalized, bad)
	}
}

// corruptAndExpectBreaks changes a's materialized balance behind the ledger's back and leaves the rail's clearing
// account with a balance at the close of November 3 (Q-127 e): the controls must open their breaks, the clearing
// account's on the second run that still finds it.
func (j *journey) corruptAndExpectBreaks(t *testing.T, a depositAccount) {
	t.Helper()
	pgtest.AsOwner(t, j.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE ledger.balances SET posted = posted - 1 WHERE account_id = $1`, a.LedgerAccountID)
		return err
	})
	expect(t, j.as("operator-a", "operator"), http.MethodPost, "/v1/transactions",
		posting("2026-11-03", j.accounts["A.CASH.VAULT"], j.accounts["L.CLEARING.SIMBREB"]), http.StatusCreated, nil)
	j.rollover(t, november3)
	for range 2 {
		if err := j.jobs.reconciliation.RunControls(t.Context(), j.uow); err != nil {
			t.Fatal(err)
		}
	}
	open := j.openBreaks(t)
	for _, control := range []string{"balances_match_entries", "customers_match_control", "rail_accounts_at_zero"} {
		if !slices.Contains(open, control) {
			t.Errorf("no open break of %s; open: %v", control, open)
		}
	}
}
