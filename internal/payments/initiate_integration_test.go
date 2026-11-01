//go:build integration

package payments_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/compliance"
	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/party"
	"github.com/DrewGGM/aluna/internal/payments"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
	"github.com/DrewGGM/aluna/internal/pricing"
	"github.com/DrewGGM/aluna/internal/products"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, &testPool)) }

// world is an entity with a savings account funded with 1,000,000 COP, the ledger accounts its product names
// and the clearing account of the simulated rail.
type world struct {
	entity   ids.ID
	ledger   *ledger.Service
	deposits *deposits.Service
	pricing  *pricing.Service
	payments *payments.Service
	account  deposits.Account
}

func cop(t *testing.T, units int64) money.Amount {
	t.Helper()
	c, err := money.CurrencyOf("COP")
	if err != nil {
		t.Fatal(err)
	}
	return money.New(units, c)
}

func do(t *testing.T, fn func(tx postgres.Tx) error) error {
	t.Helper()
	return postgres.NewUnitOfWork(testPool).Do(t.Context(), fn)
}

func mustDo(t *testing.T, fn func(tx postgres.Tx) error) {
	t.Helper()
	if err := do(t, fn); err != nil {
		t.Fatal(err)
	}
}

// newWorld is a world of the savings product, with the rail's clearing account if clearing.
func newWorld(t *testing.T, clearing bool) world {
	t.Helper()
	return worldOf(t, "valid-savings.json", clearing)
}

// worldOf is a world whose account is of the product of a products testdata file.
func worldOf(t *testing.T, file string, clearing bool) world {
	t.Helper()
	w := world{entity: ids.New()}
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO calendar.legal_entities (id, name, business_date, functional_currency)
			VALUES ($1, 'Entity', '2026-03-02', 'COP')`, w.entity)
		return err
	})
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	screening := compliance.New(events)
	parties, catalog := party.New(events, screening.PartyScreener()), products.New(events)
	w.ledger = ledger.New(calendar.New(), events)
	w.pricing = pricing.New(catalog, calendar.New(), pricing.SingleAccount)
	w.deposits = deposits.New(w.ledger, parties, catalog, calendar.New(), w.pricing, events)
	if w.payments, err = payments.New(w.ledger, w.deposits, screening, w.pricing, events, testPool); err != nil {
		t.Fatal(err)
	}
	w.accounts(t, clearing)
	w.account = w.fundedAccount(t, parties, catalog, file)
	return w
}

// accounts creates the ledger accounts the savings products name and, if clearing, the rail's.
func (w world) accounts(t *testing.T, clearing bool) {
	t.Helper()
	type account struct {
		code string
		kind ledger.Kind
		mode ledger.BalanceMode
	}
	all := []account{
		{"A.CASH.VAULT", ledger.KindAsset, ledger.BalanceModeSync},
		{"I.FEES.SAVINGS", ledger.KindIncome, ledger.BalanceModeAsync},
		{"I.FEES.TIERED", ledger.KindIncome, ledger.BalanceModeAsync},
		{"L.TAX.GMF", ledger.KindLiability, ledger.BalanceModeAsync},
	}
	if clearing {
		all = append(all, account{"L.CLEARING.SIMBREB", ledger.KindLiability, ledger.BalanceModeAsync})
	}
	mustDo(t, func(tx postgres.Tx) error {
		for _, a := range all {
			if _, err := w.ledger.CreateAccount(t.Context(), tx, ledger.CreateAccountInput{
				LegalEntityID: w.entity, Code: a.code, Kind: a.kind, Currency: cop(t, 0).Currency(), Mode: a.mode,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// fundedAccount opens an account of the product of file for an approved party and deposits 1,000,000 COP into
// it.
func (w world) fundedAccount(t *testing.T, parties *party.Service, catalog *products.Service, file string) deposits.Account {
	t.Helper()
	definition, err := os.ReadFile(filepath.Join("..", "products", "internal", "domain", "testdata", file)) //nolint:gosec // products' test fixtures
	if err != nil {
		t.Fatal(err)
	}
	var a deposits.Account
	mustDo(t, func(tx postgres.Tx) error {
		p, err := parties.Register(t.Context(), tx, party.RegisterInput{
			LegalEntityID: w.entity, Kind: party.KindNatural, ExternalRef: "onboarding-" + ids.New().String(), RiskLevel: party.RiskLow,
		})
		if err != nil {
			return err
		}
		if _, err = parties.ChangeKYC(t.Context(), tx, w.entity, p.ID, 1, party.KYCApproved); err != nil {
			return err
		}
		product, err := catalog.CreateProduct(t.Context(), tx, w.entity, "p"+strings.ReplaceAll(ids.New().String(), "-", ""), "Savings")
		if err != nil {
			return err
		}
		v, err := catalog.CreateVersion(t.Context(), tx, w.entity, product.ID, "designer", definition)
		if err != nil {
			return err
		}
		if _, err = catalog.RequestPublication(t.Context(), tx, w.entity, v.ID, "designer"); err != nil {
			return err
		}
		if _, err = catalog.Publish(t.Context(), tx, w.entity, v.ID); err != nil {
			return err
		}
		if a, err = w.deposits.Open(t.Context(), tx, deposits.OpenInput{LegalEntityID: w.entity, PartyID: p.ID, ProductID: product.ID}); err != nil {
			return err
		}
		_, err = w.deposits.Deposit(t.Context(), tx, deposits.CashInput{
			LegalEntityID: w.entity, AccountID: a.ID, Amount: cop(t, 100000000), ExternalRef: ids.New().String(), Actor: "teller", Channel: "branch",
		})
		return err
	})
	return a
}

// input is a payment of units from the world's account to the beneficiary with key.
func (w world) input(t *testing.T, units int64, key string) payments.InitiateInput {
	t.Helper()
	return payments.InitiateInput{
		LegalEntityID: w.entity, AccountID: w.account.ID, Amount: cop(t, units), Rail: payments.RailSimBreB,
		ExternalRef: ids.New().String(), Creditor: payments.AccountRef{Key: key},
	}
}

// initiate initiates in in its own transaction.
func (w world) initiate(t *testing.T, in payments.InitiateInput) (payments.Order, error) {
	t.Helper()
	var o payments.Order
	err := do(t, func(tx postgres.Tx) error {
		var err error
		o, err = w.payments.Initiate(t.Context(), tx, in)
		return err
	})
	return o, err
}

// balance is the world account's materialized balance.
func (w world) balance(t *testing.T) ledger.Balance {
	t.Helper()
	b, err := w.ledger.Balance(t.Context(), testPool, w.account.LedgerAccountID)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// stored is an order as payments.orders keeps it.
type stored struct {
	status, endToEnd    string
	holdID              *ids.ID
	heldUnits, gmfUnits int64
}

func storedOrder(t *testing.T, id ids.ID) stored {
	t.Helper()
	var s stored
	if err := testPool.QueryRow(t.Context(), `SELECT status, end_to_end_id, hold_id, held_units, gmf_units FROM payments.orders WHERE id = $1`, id).
		Scan(&s.status, &s.endToEnd, &s.holdID, &s.heldUnits, &s.gmfUnits); err != nil {
		t.Fatal(err)
	}
	return s
}

// sendJobs are the order ids of the payments.send jobs in the payments queue.
func sendJobs(t *testing.T) map[ids.ID]bool {
	t.Helper()
	rows, err := testPool.Query(t.Context(), `SELECT args FROM infra.river_job WHERE kind = 'payments.send' AND queue = 'payments'`)
	if err != nil {
		t.Fatal(err)
	}
	args, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		t.Fatal(err)
	}
	queued := make(map[ids.ID]bool)
	for _, a := range args {
		var job struct {
			OrderID ids.ID `json:"order_id"`
		}
		if err := json.Unmarshal(a, &job); err != nil {
			t.Fatal(err)
		}
		queued[job.OrderID] = true
	}
	return queued
}

// TestACleanPaymentIsHeldAndQueued (FR-31): a payment whose beneficiary screening clears holds its amount and
// its GMF (4 per thousand, no fee on a transfer of this product) against the rail's clearing account, keeps
// them, and is queued to be sent: 300,000 COP hold 301,200.
func TestACleanPaymentIsHeldAndQueued(t *testing.T) {
	w := newWorld(t, true)

	o, err := w.initiate(t, w.input(t, 30000000, "@beneficiary"))

	if err != nil || o.Status != payments.StatusHeld || o.HoldID == (ids.ID{}) || o.GMF != cop(t, 120000) || len(o.Fees) != 0 {
		t.Fatalf("a clean payment = %+v (%v), want it held with its GMF of 1,200 COP", o, err)
	}
	if b := w.balance(t); b.PendingDebits != 30120000 || o.Held != cop(t, 30120000) {
		t.Fatalf("the account's pending debits = %d and the order holds %v, want 30,120,000 held", b.PendingDebits, o.Held)
	}
	var counter string
	if err := testPool.QueryRow(t.Context(), `SELECT a.code FROM ledger.holds h JOIN ledger.accounts a ON a.id = h.counter_account_id
		WHERE h.id = $1`, o.HoldID).Scan(&counter); err != nil || counter != "L.CLEARING.SIMBREB" {
		t.Fatalf("the hold's counterparty = %q (%v), want L.CLEARING.SIMBREB", counter, err)
	}
	expectStoredHeld(t, o)
}

// eventsOf are the types of the events about order id, in the order they were emitted.
func eventsOf(t *testing.T, id ids.ID) []string {
	t.Helper()
	rows, err := testPool.Query(t.Context(), `SELECT type FROM infra.outbox WHERE subject = $1 ORDER BY id`, "payment-orders/"+id.String())
	if err != nil {
		t.Fatal(err)
	}
	types, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return types
}

// expectStoredHeld checks that held order o is stored with its hold and GMF, published and queued to be sent.
func expectStoredHeld(t *testing.T, o payments.Order) {
	t.Helper()
	if got := eventsOf(t, o.ID); !slices.Equal(got, []string{"aluna.payments.order.initiated.v1", "aluna.payments.order.held.v1"}) {
		t.Fatalf("events = %v, want initiated and held", got)
	}
	s := storedOrder(t, o.ID)
	if s.status != "held" || s.endToEnd != o.EndToEndID || s.holdID == nil || *s.holdID != o.HoldID || s.gmfUnits != o.GMF.Units() ||
		s.heldUnits != o.Held.Units() {
		t.Fatalf("stored %+v, want the held order with its hold, what it holds and its GMF", s)
	}
	if !sendJobs(t)[o.ID] {
		t.Fatal("the order was not queued to be sent")
	}
}

// TestAPaymentHoldsItsFees (FR-28, FR-31): a payment from an account whose product charges 3,000 COP a
// transfer holds that fee too, besides its amount and its GMF, and keeps it to charge it.
func TestAPaymentHoldsItsFees(t *testing.T) {
	w := worldOf(t, "valid-tiered-savings.json", true)

	o, err := w.initiate(t, w.input(t, 30000000, "@beneficiary"))

	if err != nil || len(o.Fees) != 1 || o.Fees[0].Code != "transfer_fee" || o.Fees[0].Amount != cop(t, 300000) {
		t.Fatalf("a payment of the tiered product = %+v (%v), want its transfer fee of 3,000 COP", o, err)
	}
	if b := w.balance(t); b.PendingDebits != 30000000+300000+o.GMF.Units() {
		t.Fatalf("the account's pending debits = %d, want the amount, the fee and the GMF %v", b.PendingDebits, o.GMF)
	}
	var fees string
	if err := testPool.QueryRow(t.Context(), `SELECT fees::text FROM payments.orders WHERE id = $1`, o.ID).Scan(&fees); err != nil ||
		fees != `[{"code": "transfer_fee", "units": 300000}]` {
		t.Fatalf("stored fees = %s (%v), want the transfer fee", fees, err)
	}
}

// TestAPaymentScreeningDoesNotClearMovesNothing (AB-6, FR-42, US-12): a payment to a beneficiary that may
// match, by key or by account, or while screening is down, is pending_screening: nothing is held, nothing charged, nothing queued,
// and the account's available balance is intact.
func TestAPaymentScreeningDoesNotClearMovesNothing(t *testing.T) {
	w := newWorld(t, true)

	for _, creditor := range []payments.AccountRef{{Key: "error-screening-down"}, {Key: "match-sanctioned"}, {Number: "match-1234567"}} {
		in := w.input(t, 30000000, "")
		in.Creditor = creditor
		o, err := w.initiate(t, in)

		if err != nil || o.Status != payments.StatusPendingScreening || o.HoldID != (ids.ID{}) || !o.GMF.IsZero() {
			t.Fatalf("a payment to %+v = %+v (%v), want it pending_screening without a hold", creditor, o, err)
		}
		if b := w.balance(t); b.PendingDebits != 0 || b.Posted != -100000000 {
			t.Fatalf("after a payment to %+v the balance = %+v, want 1,000,000 COP available", creditor, b)
		}
		expectStoredPending(t, o)
	}
}

// expectStoredPending checks that order o is stored pending_screening, published as initiated and not queued.
func expectStoredPending(t *testing.T, o payments.Order) {
	t.Helper()
	if sendJobs(t)[o.ID] || storedOrder(t, o.ID).status != "pending_screening" {
		t.Fatalf("the payment %s was queued or not stored pending_screening", o.ID)
	}
	if got := eventsOf(t, o.ID); !slices.Equal(got, []string{"aluna.payments.order.initiated.v1"}) {
		t.Fatalf("events of the payment %s = %v, want only initiated", o.ID, got)
	}
}

// TestAQuotedPaymentHoldsWhatWasQuoted (FR-28, FR-31): a payment with its quote holds what the quote said, and
// the quote is used: it does not price a second payment.
func TestAQuotedPaymentHoldsWhatWasQuoted(t *testing.T) {
	w := newWorld(t, true)
	q := w.quote(t, 20000000)
	in := w.input(t, 20000000, "@beneficiary")
	in.QuoteID = q.ID

	o, err := w.initiate(t, in)
	_, again := w.initiate(t, in)

	if err != nil || o.GMF != q.GMF.Tax || w.balance(t).PendingDebits != 20000000+q.GMF.Tax.Units() {
		t.Fatalf("a quoted payment = %+v (%v), want the quote's GMF %v held", o, err, q.GMF.Tax)
	}
	if !errors.Is(again, pricing.ErrQuoteUnavailable) {
		t.Fatalf("the same quote again = %v, want ErrQuoteUnavailable", again)
	}
}

// quote quotes, within its own transaction, a transfer of units from the world's account.
func (w world) quote(t *testing.T, units int64) pricing.Quote {
	t.Helper()
	var q pricing.Quote
	mustDo(t, func(tx postgres.Tx) error {
		in, err := w.deposits.QuoteInput(t.Context(), tx, w.entity, w.account.ID, pricing.Transfer, cop(t, units))
		if err != nil {
			return err
		}
		q, err = w.pricing.Quote(t.Context(), tx, in)
		return err
	})
	return q
}

// TestAPendingPaymentLeavesItsQuote (AB-6, FR-28): a payment waiting for compliance does not use its quote: a
// payment that clears can still use it.
func TestAPendingPaymentLeavesItsQuote(t *testing.T) {
	w := newWorld(t, true)
	q := w.quote(t, 20000000)
	pending := w.input(t, 20000000, "match-sanctioned")
	pending.QuoteID = q.ID
	if _, err := w.initiate(t, pending); err != nil {
		t.Fatal(err)
	}
	clean := w.input(t, 20000000, "@beneficiary")
	clean.QuoteID = q.ID

	if _, err := w.initiate(t, clean); err != nil {
		t.Fatalf("a clean payment with the quote a pending one named = %v, want it held", err)
	}
}

// TestAPaymentTheCoreDoesNotTake (FR-31): a payment without funds for its amount and charges, from an account
// that is not active or of another entity, in another currency, to a rail it does not know, or to a
// beneficiary that is not one account or one key, is rejected, and leaves nothing.
func TestAPaymentTheCoreDoesNotTake(t *testing.T) {
	w := newWorld(t, true)
	usd, err := money.CurrencyOf("USD")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		change func(*payments.InitiateInput)
		want   error
	}{
		"no funds":         {func(in *payments.InitiateInput) { in.Amount = cop(t, 99900000) }, ledger.ErrInsufficientFunds},
		"another entity":   {func(in *payments.InitiateInput) { in.LegalEntityID = ids.New() }, deposits.ErrAccountNotFound},
		"another currency": {func(in *payments.InitiateInput) { in.Amount = money.New(1000, usd) }, payments.ErrInvalidOrder},
		"an unknown rail":  {func(in *payments.InitiateInput) { in.Rail = "swift" }, payments.ErrInvalidOrder},
		"no reference":     {func(in *payments.InitiateInput) { in.ExternalRef = "" }, payments.ErrInvalidOrder},
		"a key of 129":     {func(in *payments.InitiateInput) { in.Creditor.Key = strings.Repeat("k", 129) }, payments.ErrInvalidOrder},
		"an account of 65": {func(in *payments.InitiateInput) {
			in.Creditor = payments.AccountRef{Number: strings.Repeat("1", 65)}
		}, payments.ErrInvalidOrder},
		"a key with NUL":         {func(in *payments.InitiateInput) { in.Creditor.Key = "@be\x00neficiary" }, payments.ErrInvalidOrder},
		"no beneficiary":         {func(in *payments.InitiateInput) { in.Creditor = payments.AccountRef{} }, payments.ErrInvalidOrder},
		"a beneficiary two ways": {func(in *payments.InitiateInput) { in.Creditor.Number = "123" }, payments.ErrInvalidOrder},
	}
	for name, tc := range cases {
		in := w.input(t, 30000000, "@beneficiary")
		tc.change(&in)
		if _, err := w.initiate(t, in); !errors.Is(err, tc.want) {
			t.Errorf("%s = %v, want %v", name, err, tc.want)
		}
	}
	mustDo(t, func(tx postgres.Tx) error {
		return w.deposits.ChangeStatus(t.Context(), tx, w.entity, w.account.ID, deposits.Frozen, "compliance-officer")
	})
	for _, key := range []string{"@beneficiary", "match-sanctioned"} {
		if _, err := w.initiate(t, w.input(t, 30000000, key)); !errors.Is(err, deposits.ErrAccountNotActive) {
			t.Errorf("from a frozen account to %s = %v, want ErrAccountNotActive, before screening", key, err)
		}
	}
	var orders int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM payments.orders WHERE account_id = $1`, w.account.ID).Scan(&orders); err != nil {
		t.Fatal(err)
	}
	if b := w.balance(t); b.PendingDebits != 0 || orders != 0 {
		t.Fatalf("after the rejected payments the balance = %+v with %d orders, want nothing held or stored", b, orders)
	}
}

// TestAFreezeDuringAPaymentStopsIt (FR-31): an account frozen while a payment from it is being made, before
// it is held, is not held: the payment waits for the freeze and then finds the account not active.
func TestAFreezeDuringAPaymentStopsIt(t *testing.T) {
	w := newWorld(t, true)
	paid, in := make(chan error, 1), w.input(t, 30000000, "@beneficiary")

	err := do(t, func(tx postgres.Tx) error {
		if err := w.deposits.ChangeStatus(t.Context(), tx, w.entity, w.account.ID, deposits.Frozen, "compliance-officer"); err != nil {
			return err
		}
		go func() {
			err := do(t, func(tx postgres.Tx) error {
				_, err := w.payments.Initiate(t.Context(), tx, in)
				return err
			})
			paid <- err
		}()
		select {
		case err := <-paid:
			return errors.Join(errors.New("the payment did not wait for the freeze"), err)
		case <-time.After(300 * time.Millisecond):
			return nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := <-paid; !errors.Is(err, deposits.ErrAccountNotActive) || w.balance(t).PendingDebits != 0 {
		t.Fatalf("a payment during a freeze = %v with %d held, want ErrAccountNotActive and nothing held", err, w.balance(t).PendingDebits)
	}
}

// TestARailWithoutClearingAccountIsNotConfigured (FR-31): an entity without the rail's clearing account, or
// with one in another currency, cannot pay through it: ErrRailNotConfigured.
func TestARailWithoutClearingAccountIsNotConfigured(t *testing.T) {
	w := newWorld(t, false)
	if _, err := w.initiate(t, w.input(t, 30000000, "@beneficiary")); !errors.Is(err, payments.ErrRailNotConfigured) {
		t.Fatalf("a payment without the clearing account = %v, want ErrRailNotConfigured", err)
	}
	usd, err := money.CurrencyOf("USD")
	if err != nil {
		t.Fatal(err)
	}
	mustDo(t, func(tx postgres.Tx) error {
		_, err := w.ledger.CreateAccount(t.Context(), tx, ledger.CreateAccountInput{
			LegalEntityID: w.entity, Code: "L.CLEARING.SIMBREB", Kind: ledger.KindLiability, Currency: usd, Mode: ledger.BalanceModeAsync,
		})
		return err
	})

	if _, err := w.initiate(t, w.input(t, 30000000, "@beneficiary")); !errors.Is(err, payments.ErrRailNotConfigured) {
		t.Fatalf("a payment with a clearing account in USD = %v, want ErrRailNotConfigured", err)
	}
}

// TestAReferenceIsOnePayment (FR-31): a second payment with a reference the entity already used is
// ErrDuplicatePayment and holds nothing more.
func TestAReferenceIsOnePayment(t *testing.T) {
	w := newWorld(t, true)
	in := w.input(t, 30000000, "@beneficiary")
	if _, err := w.initiate(t, in); err != nil {
		t.Fatal(err)
	}

	_, again := w.initiate(t, in)

	if !errors.Is(again, payments.ErrDuplicatePayment) || w.balance(t).PendingDebits != 30120000 {
		t.Fatalf("the same reference again = %v with %d held, want ErrDuplicatePayment and the first hold only", again, w.balance(t).PendingDebits)
	}
}

// casesOf counts within tx the screening cases of entity.
func casesOf(t *testing.T, tx postgres.Tx, entity ids.ID) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM compliance.cases WHERE legal_entity_id = $1`, entity).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestARejectedPaymentLeavesNothingInItsTransaction (FR-31, FR-42): a payment rejected for lack of funds
// leaves nothing in the caller's transaction, not its screening case, and the transaction goes on.
func TestARejectedPaymentLeavesNothingInItsTransaction(t *testing.T) {
	w := newWorld(t, true)

	mustDo(t, func(tx postgres.Tx) error {
		before := casesOf(t, tx, w.entity)
		if _, err := w.payments.Initiate(t.Context(), tx, w.input(t, 99900000, "@beneficiary")); !errors.Is(err, ledger.ErrInsufficientFunds) {
			t.Errorf("a payment without funds = %v, want ErrInsufficientFunds", err)
		}
		if after := casesOf(t, tx, w.entity); after != before {
			t.Errorf("screening cases in the transaction went from %d to %d, want none left", before, after)
		}
		_, err := w.payments.Initiate(t.Context(), tx, w.input(t, 30000000, "@beneficiary"))
		return err
	})
}

// asOwner runs sql as the tables' owner and returns its error.
func asOwner(t *testing.T, sql string, args ...any) error {
	t.Helper()
	return pgx.BeginFunc(t.Context(), testPool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), "SET LOCAL ROLE NONE"); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), sql, args...)
		return err
	})
}

// sqlState is the SQLSTATE of err with its constraint, or "never deleted" for the guard's refusal to delete;
// "" if it is not a database error.
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	switch {
	case !errors.As(err, &pgErr):
		return ""
	case strings.Contains(pgErr.Message, "never deleted"):
		return pgErr.Code + " never deleted"
	}
	return pgErr.Code + " " + pgErr.ConstraintName
}

// TestTheDatabaseKeepsTheOrders (FR-30): an end-to-end id is of one order, no order is deleted or truncated,
// only where it stands changes (not even the owner changes what it is or, once held, what it holds), an order
// waiting for compliance holds nothing, and the core can move an order on and hold one that waited.
func TestTheDatabaseKeepsTheOrders(t *testing.T) {
	w := newWorld(t, true)
	o, err := w.initiate(t, w.input(t, 30000000, "@beneficiary"))
	if err != nil {
		t.Fatal(err)
	}
	pending, err := w.initiate(t, w.input(t, 30000000, "match-sanctioned"))
	if err != nil {
		t.Fatal(err)
	}
	_, coreChangesAmount := testPool.Exec(t.Context(), `UPDATE payments.orders SET amount_units = 1 WHERE id = $1`, o.ID)
	cases := map[string]struct {
		err  error
		want string
	}{
		"a repeated end-to-end id": {asOwner(t, `INSERT INTO payments.orders (id, legal_entity_id, account_id, direction, rail, end_to_end_id,
			instr_id, tx_id, currency, amount_units, creditor_key, status, hold_id, external_ref) SELECT gen_random_uuid(), legal_entity_id,
			account_id, direction, rail, end_to_end_id, instr_id, tx_id, currency, amount_units, creditor_key, status, hold_id,
			gen_random_uuid()::text FROM payments.orders WHERE id = $1`, o.ID), "23505 orders_end_to_end_id_unique"},
		"delete":                   {asOwner(t, `DELETE FROM payments.orders WHERE id = $1`, o.ID), "2F003 never deleted"},
		"truncate":                 {asOwner(t, `TRUNCATE payments.orders`), "2F003 never deleted"},
		"the owner changes amount": {asOwner(t, `UPDATE payments.orders SET amount_units = 1 WHERE id = $1`, o.ID), "2F003 "},
		"the core changes amount":  {coreChangesAmount, "42501 "},
		"a held order that changes what it holds": {
			asOwner(t, `UPDATE payments.orders SET held_units = 1 WHERE id = $1`, o.ID), "2F003 ",
		},
		"a pending order rejected with what it holds": {
			asOwner(t, `UPDATE payments.orders SET status = 'rejected', held_units = 1 WHERE id = $1`, pending.ID), "2F003 ",
		},
		"a held order back to pending screening": {
			asOwner(t, `UPDATE payments.orders SET status = 'pending_screening' WHERE id = $1`, o.ID), "23514 orders_pending_holds_nothing",
		},
	}
	for name, tc := range cases {
		if got := sqlState(tc.err); got != tc.want {
			t.Errorf("%s = %q (%v), want %q", name, got, tc.err, tc.want)
		}
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE payments.orders SET status = 'sent', rail_ref = 'SIM1' WHERE id = $1`, o.ID); err != nil {
		t.Fatalf("the core moving the order on = %v, want it taken", err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE payments.orders SET status = 'held', hold_id = $2, held_units = 30120000, gmf_units = 120000
		WHERE id = $1`, pending.ID, o.HoldID); err != nil {
		t.Fatalf("the core holding an order that waited for compliance = %v, want it taken", err)
	}
}
