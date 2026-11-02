//go:build integration

package payments_test

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/payments"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// settledOn is the interbank settlement date of the test transfers.
var settledOn = time.Date(2026, time.March, 2, 0, 0, 0, 0, time.UTC)

// transfer is a transfer of units from BANKXCOBB, with an end-to-end id of its own, to creditor in the world's
// entity.
func (w world) transfer(t *testing.T, units int64, creditor payments.AccountRef) payments.InboundTransfer {
	t.Helper()
	e2e := "BANKX" + strings.ToUpper(strings.ReplaceAll(ids.New().String(), "-", ""))[:30]
	return payments.InboundTransfer{
		LegalEntityID: w.entity, EndToEndID: e2e, InstrID: "INSTR1", TxID: "TX1", RailRef: "SIMIN" + e2e[5:], DebtorAgent: "BANKXCOBB",
		Amount: cop(t, units), SettlementDate: settledOn, Creditor: creditor,
	}
}

func (w world) byNumber() payments.AccountRef {
	return payments.AccountRef{Number: w.account.ID.String()}
}

// receive takes in in its own transaction.
func (w world) receive(t *testing.T, in payments.InboundTransfer) error {
	t.Helper()
	return do(t, func(tx postgres.Tx) error { return w.payments.ReceiveTransfer(t.Context(), tx, in) })
}

// received is an incoming order as payments.orders keeps it.
type received struct {
	id                      ids.ID
	status, reason, railRef string
	accountID               *ids.ID
}

// receivedOrders are the incoming orders of the world's entity from in's bank with in's end-to-end id.
func (w world) receivedOrders(t *testing.T, in payments.InboundTransfer) []received {
	t.Helper()
	rows, err := testPool.Query(t.Context(), `SELECT id, status, coalesce(reason, ''), coalesce(rail_ref, ''), account_id
		FROM payments.orders WHERE legal_entity_id = $1 AND direction = 'incoming' AND debtor_agent = $2 AND end_to_end_id = $3`,
		w.entity, in.DebtorAgent, in.EndToEndID)
	if err != nil {
		t.Fatal(err)
	}
	orders, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (received, error) {
		var r received
		return r, row.Scan(&r.id, &r.status, &r.reason, &r.railRef, &r.accountID)
	})
	if err != nil {
		t.Fatal(err)
	}
	return orders
}

// only is the one incoming order of in.
func (w world) only(t *testing.T, in payments.InboundTransfer) received {
	t.Helper()
	orders := w.receivedOrders(t, in)
	if len(orders) != 1 {
		t.Fatalf("%d orders of the transfer, want one", len(orders))
	}
	return orders[0]
}

// booked are the units the transfers received post to each account of the world's entity (its customer's is
// "customer"), the number of their transactions and their value dates.
func (w world) booked(t *testing.T) (map[string]int64, int, []time.Time) {
	t.Helper()
	rows, err := testPool.Query(t.Context(), `SELECT CASE WHEN a.id = $2 THEN 'customer' ELSE a.code END, e.amount, t.id, t.value_date
		FROM ledger.entries e JOIN ledger.transactions t ON t.id = e.transaction_id JOIN ledger.accounts a ON a.id = e.account_id
		WHERE t.legal_entity_id = $1 AND t.code IN ('payments.inbound_credit', 'payments.inbound_suspense')`, w.entity, w.account.LedgerAccountID)
	if err != nil {
		t.Fatal(err)
	}
	units, dates := map[string]int64{}, map[ids.ID]time.Time{}
	var (
		account string
		amount  int64
		tx      ids.ID
		valued  time.Time
	)
	if _, err := pgx.ForEachRow(rows, []any{&account, &amount, &tx, &valued}, func() error {
		units[account] += amount
		dates[tx] = valued
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return units, len(dates), slices.Collect(maps.Values(dates))
}

// TestATransferToAnActiveAccountIsCredited (FR-32): a transfer for an active account, by its number, in its
// currency, debits the rail's clearing account and credits the customer on its settlement date; the order
// settles with the rail's reference and is published.
func TestATransferToAnActiveAccountIsCredited(t *testing.T) {
	w := newWorld(t, true)
	in := w.transfer(t, 30000000, w.byNumber())

	if err := w.receive(t, in); err != nil {
		t.Fatal(err)
	}

	o := w.only(t, in)
	expectReceived(t, "the transfer", o, "settled", "", &w.account.ID)
	if o.railRef != in.RailRef {
		t.Fatalf("rail reference = %q, want the rail's", o.railRef)
	}
	units, transactions, dates := w.booked(t)
	if want := map[string]int64{"customer": -30000000, "L.CLEARING.SIMBREB": 30000000}; !maps.Equal(units, want) || transactions != 1 ||
		!dates[0].Equal(settledOn) {
		t.Fatalf("booked %v in %d transactions on %v, want %v on the settlement date", units, transactions, dates, want)
	}
	if b := w.balance(t); b.Posted != -130000000 {
		t.Fatalf("balance = %+v, want 1,300,000 COP", b)
	}
}

// expectReceived checks that o, the order of the transfer what, is at status for reason, for account (nil:
// for none), and published as such.
func expectReceived(t *testing.T, what string, o received, status, reason string, account *ids.ID) {
	t.Helper()
	if o.status != status || o.reason != reason || !slices.Equal(eventsOf(t, o.id), []string{"aluna.payments.order." + status + ".v1"}) {
		t.Fatalf("%s: order = %+v, events %v; want it %s for %q and published", what, o, eventsOf(t, o.id), status, reason)
	}
	if (o.accountID == nil) != (account == nil) || (account != nil && *o.accountID != *account) {
		t.Fatalf("%s: order for account %v, want %v", what, o.accountID, account)
	}
}

// TestATransferNoAccountTakesGoesToSuspense (FR-32): one by key (no directory yet), for a number that is no
// account, or no account of the entity, for an account that is not active or in another currency debits the
// clearing account and credits the rail's suspense account; the order is suspended saying why, keeping the
// account if it exists, and published: the exception to resolve.
func TestATransferNoAccountTakesGoesToSuspense(t *testing.T) {
	w, other := newWorld(t, true), newWorld(t, true)
	frozen := newWorld(t, true)
	mustDo(t, func(tx postgres.Tx) error {
		return frozen.deposits.ChangeStatus(t.Context(), tx, frozen.entity, frozen.account.ID, deposits.Frozen, "compliance-officer")
	})
	cases := map[string]struct {
		w        world
		creditor payments.AccountRef
		reason   string
		account  *ids.ID
	}{
		"by key":                   {w, payments.AccountRef{Key: "@customer"}, payments.ReasonKeyNotResolved, nil},
		"an unknown account":       {w, payments.AccountRef{Number: ids.New().String()}, payments.ReasonUnknownAccount, nil},
		"a number that is no id":   {w, payments.AccountRef{Number: "123456789"}, payments.ReasonUnknownAccount, nil},
		"another entity's account": {w, other.byNumber(), payments.ReasonUnknownAccount, nil},
		"a frozen account":         {frozen, frozen.byNumber(), payments.ReasonAccountNotActive, &frozen.account.ID},
		"an active account":        {w, w.byNumber(), "", &w.account.ID},
	}
	for name, tc := range cases {
		in := tc.w.transfer(t, 1000000, tc.creditor)
		if err := tc.w.receive(t, in); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		status := "suspended"
		if tc.reason == "" {
			status = "settled"
		}
		expectReceived(t, name, tc.w.only(t, in), status, tc.reason, tc.account)
	}
	units, _, _ := w.booked(t)
	if want := map[string]int64{"L.CLEARING.SIMBREB": 5000000, "L.SUSPENSE.SIMBREB": -4000000, "customer": -1000000}; !maps.Equal(units, want) {
		t.Fatalf("booked %v, want 40,000 COP to suspense and 10,000 to the customer", units)
	}
	if units, _, _ := frozen.booked(t); units["L.SUSPENSE.SIMBREB"] != -1000000 || frozen.balance(t).Posted != -100000000 {
		t.Fatalf("booked %v for the frozen account, want it in suspense and the account untouched", units)
	}
}

// TestATransferInAnotherCurrencyGoesToSuspense (FR-32): a transfer in dollars for an account in pesos goes to
// the rail's suspense account in dollars.
func TestATransferInAnotherCurrencyGoesToSuspense(t *testing.T) {
	w := newWorld(t, false)
	usd, err := money.CurrencyOf("USD")
	if err != nil {
		t.Fatal(err)
	}
	mustDo(t, func(tx postgres.Tx) error {
		for _, code := range []string{"L.CLEARING.SIMBREB", "L.SUSPENSE.SIMBREB"} {
			if _, err := w.ledger.CreateAccount(t.Context(), tx, ledger.CreateAccountInput{
				LegalEntityID: w.entity, Code: code, Kind: ledger.KindLiability, Currency: usd, Mode: ledger.BalanceModeAsync,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	in := w.transfer(t, 50000, w.byNumber())
	in.Amount = money.New(50000, usd)

	if err := w.receive(t, in); err != nil {
		t.Fatal(err)
	}

	expectReceived(t, "a transfer in dollars", w.only(t, in), "suspended", payments.ReasonAccountInAnotherCurrency, &w.account.ID)
	if units, _, _ := w.booked(t); units["L.SUSPENSE.SIMBREB"] != -50000 || w.balance(t).Posted != -100000000 {
		t.Fatalf("booked %v, want 500 USD in suspense and the account untouched", units)
	}
	var code string
	if err := testPool.QueryRow(t.Context(), `SELECT code FROM ledger.transactions WHERE legal_entity_id = $1 AND source = 'payments'`,
		w.entity).Scan(&code); err != nil || code != "payments.inbound_suspense" {
		t.Fatalf("transaction code = %q (%v), want payments.inbound_suspense", code, err)
	}
}

// TestATransferIsReceivedOnce (FR-32, TH-7): the same transfer again books nothing; another bank's with the same
// end-to-end id is another transfer; and one with an outgoing order's end-to-end id leaves that order alone,
// whose outcome still finds it.
func TestATransferIsReceivedOnce(t *testing.T) {
	w := newWorld(t, true)
	in := w.transfer(t, 30000000, w.byNumber())
	sent := w.sentOrder(t, 30000000)
	fromOther, likeSent := in, w.transfer(t, 1000000, w.byNumber())
	fromOther.DebtorAgent = "BANKYCOBB"
	likeSent.EndToEndID = sent.EndToEndID

	for _, tr := range []payments.InboundTransfer{in, in, fromOther, likeSent} {
		if err := w.receive(t, tr); err != nil {
			t.Fatal(err)
		}
	}

	w.only(t, in)
	w.only(t, fromOther)
	w.only(t, likeSent)
	if units, transactions, _ := w.booked(t); units["customer"] != -61000000 || transactions != 3 {
		t.Fatalf("booked %v in %d transactions, want three transfers once each", units, transactions)
	}
	if err := w.take(t, w.rail.notification(t)); err != nil || storedOrder(t, sent.ID).status != "settled" {
		t.Fatalf("the outcome of the outgoing order = %v, want it settled", err)
	}
}

// TestATransferTheCoreDoesNotTake (FR-32): one whose identifiers, amount, creditor, debtor agent, settlement
// date or reference are invalid is ErrInvalidNotification, and one for an entity without the rail's clearing
// or suspense account ErrRailNotConfigured; none of them leaves anything.
func TestATransferTheCoreDoesNotTake(t *testing.T) {
	w, noClearing, noSuspense := newWorld(t, true), newWorld(t, false), newWorld(t, false)
	mustDo(t, func(tx postgres.Tx) error {
		_, err := noSuspense.ledger.CreateAccount(t.Context(), tx, ledger.CreateAccountInput{
			LegalEntityID: noSuspense.entity, Code: "L.CLEARING.SIMBREB", Kind: ledger.KindLiability, Currency: cop(t, 0).Currency(),
			Mode: ledger.BalanceModeAsync,
		})
		return err
	})
	cases := map[string]struct {
		w      world
		change func(*payments.InboundTransfer)
		want   error
	}{
		"an end-to-end id with a slash": {w, func(in *payments.InboundTransfer) { in.EndToEndID = "/BANKX1" }, payments.ErrInvalidNotification},
		"no tx id":                      {w, func(in *payments.InboundTransfer) { in.TxID = "" }, payments.ErrInvalidNotification},
		"a zero amount":                 {w, func(in *payments.InboundTransfer) { in.Amount = cop(t, 0) }, payments.ErrInvalidNotification},
		"no debtor agent":               {w, func(in *payments.InboundTransfer) { in.DebtorAgent = "" }, payments.ErrInvalidNotification},
		"a debtor agent with a slash":   {w, func(in *payments.InboundTransfer) { in.DebtorAgent = "BANK/A" }, payments.ErrInvalidNotification},
		"no entity":                     {w, func(in *payments.InboundTransfer) { in.LegalEntityID = ids.ID{} }, payments.ErrInvalidNotification},
		"no settlement date":            {w, func(in *payments.InboundTransfer) { in.SettlementDate = time.Time{} }, payments.ErrInvalidNotification},
		"a reference too long":          {w, func(in *payments.InboundTransfer) { in.RailRef = strings.Repeat("S", 256) }, payments.ErrInvalidNotification},
		"no creditor":                   {w, func(in *payments.InboundTransfer) { in.Creditor = payments.AccountRef{} }, payments.ErrInvalidNotification},
		"a creditor by both": {w, func(in *payments.InboundTransfer) {
			in.Creditor = payments.AccountRef{Number: w.account.ID.String(), Key: "@x"}
		}, payments.ErrInvalidNotification},
		"a key with a NUL":    {w, func(in *payments.InboundTransfer) { in.Creditor = payments.AccountRef{Key: "@\x00"} }, payments.ErrInvalidNotification},
		"no clearing account": {noClearing, func(*payments.InboundTransfer) {}, payments.ErrRailNotConfigured},
		"no suspense account": {noSuspense, func(*payments.InboundTransfer) {}, payments.ErrRailNotConfigured},
	}
	for name, tc := range cases {
		in := tc.w.transfer(t, 1000000, tc.w.byNumber())
		tc.change(&in)
		if err := tc.w.receive(t, in); !errors.Is(err, tc.want) {
			t.Errorf("%s = %v, want %v", name, err, tc.want)
		}
		if orders := tc.w.receivedOrders(t, in); len(orders) != 0 {
			t.Errorf("%s left %d orders", name, len(orders))
		}
	}
	if units, _, _ := w.booked(t); len(units) != 0 {
		t.Fatalf("booked %v, want nothing", units)
	}
}

// TestTheDatabaseKeepsIncomingOrders (FR-32): an incoming order names its debtor agent and an outgoing one does
// not, an outgoing order has its account, the same bank's end-to-end id is one incoming order, and not even the
// owner changes its debtor agent.
func TestTheDatabaseKeepsIncomingOrders(t *testing.T) {
	w := newWorld(t, true)
	in := w.transfer(t, 1000000, payments.AccountRef{Key: "@customer"})
	if err := w.receive(t, in); err != nil {
		t.Fatal(err)
	}
	o := w.only(t, in)
	cases := map[string]struct {
		err  error
		want string
	}{
		"the same bank's end-to-end id": {
			asOwner(t, `INSERT INTO payments.orders (id, legal_entity_id, account_id, direction, rail,
			end_to_end_id, instr_id, tx_id, currency, amount_units, creditor_key, status, reason, external_ref, debtor_agent)
			SELECT gen_random_uuid(), legal_entity_id, account_id, direction, rail, end_to_end_id, instr_id, tx_id, currency, amount_units,
			creditor_key, status, reason, gen_random_uuid()::text, debtor_agent FROM payments.orders WHERE id = $1`, o.id),
			"23505 orders_end_to_end_id_unique",
		},
		"incoming without debtor agent": {asOwner(t, `UPDATE payments.orders SET debtor_agent = NULL WHERE id = $1`, o.id), "2F003 "},
		"the owner changes the agent":   {asOwner(t, `UPDATE payments.orders SET debtor_agent = 'BANKY' WHERE id = $1`, o.id), "2F003 "},
		"an incoming order without agent": {asOwner(t, `INSERT INTO payments.orders (id, legal_entity_id, direction, rail, end_to_end_id,
			instr_id, tx_id, currency, amount_units, creditor_key, status, external_ref) VALUES (gen_random_uuid(), $1, 'incoming', 'simbreb',
			'E2E1', 'I1', 'T1', 'COP', 100, '@x', 'suspended', gen_random_uuid()::text)`, w.entity), "23514 orders_debtor_agent"},
		"an outgoing order with agent": {asOwner(t, `INSERT INTO payments.orders (id, legal_entity_id, account_id, direction, rail,
			end_to_end_id, instr_id, tx_id, currency, amount_units, creditor_key, status, external_ref, debtor_agent) VALUES (gen_random_uuid(),
			$1, $2, 'outgoing', 'simbreb', 'E2E2', 'I1', 'T1', 'COP', 100, '@x', 'rejected', gen_random_uuid()::text, 'BANKX')`,
			w.entity, w.account.ID), "23514 orders_debtor_agent"},
		"an outgoing order without account": {asOwner(t, `INSERT INTO payments.orders (id, legal_entity_id, direction, rail, end_to_end_id,
			instr_id, tx_id, currency, amount_units, creditor_key, status, external_ref) VALUES (gen_random_uuid(), $1, 'outgoing', 'simbreb',
			'E2E3', 'I1', 'T1', 'COP', 100, '@x', 'rejected', gen_random_uuid()::text)`, w.entity), "23514 orders_outgoing_account"},
	}
	for name, tc := range cases {
		if got := sqlState(tc.err); got != tc.want {
			t.Errorf("%s = %q (%v), want %q", name, got, tc.err, tc.want)
		}
	}
}
