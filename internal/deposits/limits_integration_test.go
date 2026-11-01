//go:build integration

package deposits_test

import (
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/party"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/products"
)

// threeSMMLV is 3 times the minimum wage of 2026 that minimumWage loads (1,423,500 pesos), in COP minor units.
const threeSMMLV = 427050000

// minimumWage loads the minimum wage of 2026, 1,423,500 pesos.
func (w world) minimumWage(t *testing.T) {
	t.Helper()
	w.marketParameter(t, products.SMMLV, "1423500")
}

// marketParameter loads a parameter of kind worth value for 2026, unless another test of the package's
// database already did: the parameters are the tenant's.
func (w world) marketParameter(t *testing.T, kind products.MarketKind, value string) {
	t.Helper()
	v, err := money.ParsePrecise(value)
	if err != nil {
		t.Fatal(err)
	}
	err = try(t, func(tx postgres.Tx) error {
		return w.products.LoadMarketParameter(t.Context(), tx, products.MarketParameter{
			Kind: kind, ValidFrom: calendar.Date{Year: 2026, Month: time.January, Day: 1},
			ValidTo: calendar.Date{Year: 2027, Month: time.January, Day: 1}, Value: v,
		})
	})
	if err != nil && !errors.Is(err, products.ErrMarketParameterOverlap) {
		t.Fatal(err)
	}
}

// simplified opens an active simplified deposit (3 SMMLV of monthly debits and of balance) for a party with
// KYC approved.
func (w world) simplified(t *testing.T) deposits.Account {
	t.Helper()
	p, _ := w.product(t, "valid-simplified-deposit.json", true)
	a, err := w.open(t, w.party(t, party.KYCApproved), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// limited opens an active account of the savings product with limits (a JSON object) instead of its own.
func (w world) limited(t *testing.T, limits string) deposits.Account {
	t.Helper()
	return w.limitedIn(t, "COP", limits)
}

// limitedIn is limited with the product in currency.
func (w world) limitedIn(t *testing.T, currency, limits string) deposits.Account {
	t.Helper()
	return w.openWith(t, map[string]string{"limits": limits, "currency": `"` + currency + `"`})
}

// moved is what an account moved in a period.
type moved struct{ debits, credits int64 }

// counters are what account id moved, by period.
func counters(t *testing.T, id ids.ID) map[string]moved {
	t.Helper()
	rows, err := testPool.Query(t.Context(), `SELECT period, debits, credits FROM deposits.counters WHERE account_id = $1`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]moved{}
	for rows.Next() {
		var period string
		var debits, credits int64
		if err := rows.Scan(&period, &debits, &credits); err != nil {
			t.Fatal(err)
		}
		got[period] = moved{debits, credits}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

// expectExceeds checks that err exceeds the limit name of units minor units.
func expectExceeds(t *testing.T, what string, err error, name string, units int64) {
	t.Helper()
	var limit *deposits.LimitError
	if !errors.Is(err, deposits.ErrLimitExceeded) || !errors.As(err, &limit) || limit.Name != name || limit.Units != units {
		t.Fatalf("%s = %v, want the %s limit of %d exceeded", what, err, name, units)
	}
}

// steps runs each step in order and stops the test at the first that fails.
func steps(t *testing.T, each ...func() error) {
	t.Helper()
	for i, step := range each {
		if err := step(); err != nil {
			t.Fatalf("step %d = %v", i, err)
		}
	}
}

func (w world) rollover(t *testing.T, from calendar.Date) {
	t.Helper()
	inTx(t, func(tx postgres.Tx) error {
		_, err := calendar.New().Rollover(t.Context(), tx, w.entity, from)
		return err
	})
}

// TestASimplifiedDepositStopsAtThreeSMMLV (FR-24, PRD edge case): a simplified deposit takes up to 3 SMMLV of
// balance, the minimum wage of the business date in pesos; the deposit that exceeds it is rejected whole and
// does not count. What it takes counts as credits of the day and of the month.
func TestASimplifiedDepositStopsAtThreeSMMLV(t *testing.T) {
	w := newWorld(t)
	w.minimumWage(t)
	w.vault(t)
	a := w.simplified(t)

	if err := w.deposit(t, a.ID, threeSMMLV); err != nil {
		t.Fatalf("deposit up to 3 SMMLV = %v", err)
	}
	expectExceeds(t, "one more peso", w.deposit(t, a.ID, 100), "max_balance", threeSMMLV)

	want := map[string]moved{"2026-03-02": {0, threeSMMLV}, "2026-03": {0, threeSMMLV}}
	if got := counters(t, a.ID); w.balance(t, a) != threeSMMLV || !maps.Equal(got, want) {
		t.Fatalf("balance %d, counters %v; want %d and %v", w.balance(t, a), got, threeSMMLV, want)
	}
}

// TestTheMonthlyDebitsAddUp (FR-24): the debits of a simplified deposit add up over the month up to 3 SMMLV,
// whatever comes in between; once there, not one more unit leaves that month, even on another day.
func TestTheMonthlyDebitsAddUp(t *testing.T) {
	w := newWorld(t)
	w.minimumWage(t)
	w.vault(t)
	a := w.simplified(t)
	steps(t,
		func() error { return w.deposit(t, a.ID, threeSMMLV) },
		func() error { return w.withdraw(t, a.ID, 300000000) },
		func() error { return w.deposit(t, a.ID, 300000000) },
		func() error { return w.withdraw(t, a.ID, threeSMMLV-300000000) },
	)
	w.rollover(t, calendar.Date{Year: 2026, Month: time.March, Day: 2})

	expectExceeds(t, "a withdrawal the next day", w.withdraw(t, a.ID, 1), "monthly_debits", threeSMMLV)
	if got := counters(t, a.ID)["2026-03"]; got != (moved{threeSMMLV, threeSMMLV + 300000000}) {
		t.Fatalf("counters of March = %v, want %d debits and %d credits", got, threeSMMLV, threeSMMLV+300000000)
	}
}

// TestTheLimitsPerOperationAndPerDay (FR-24): limits in units apply as they are; the per operation one to
// debits and credits, the daily one to debits only, and the day after it starts again.
func TestTheLimitsPerOperationAndPerDay(t *testing.T) {
	w := newWorld(t)
	w.vault(t)
	a := w.limited(t, `{"per_operation": {"units": 1000}, "daily_debits": {"units": 1500}}`)

	expectExceeds(t, "a deposit over the operation", w.deposit(t, a.ID, 1001), "per_operation", 1000)
	for i := range 3 {
		if err := w.deposit(t, a.ID, 1000); err != nil {
			t.Fatalf("deposit %d = %v: credits have no daily limit", i, err)
		}
	}
	if err := w.withdraw(t, a.ID, 1000); err != nil {
		t.Fatal(err)
	}
	expectExceeds(t, "a withdrawal over the day", w.withdraw(t, a.ID, 600), "daily_debits", 1500)
	w.rollover(t, calendar.Date{Year: 2026, Month: time.March, Day: 2})

	if err := w.withdraw(t, a.ID, 600); err != nil || w.balance(t, a) != 1400 {
		t.Fatalf("withdrawal the next day = %v, balance %d; want it accepted and 1400", err, w.balance(t, a))
	}
}

// TestATransferStaysWithinBothAccountsLimits (FR-24): a transfer is a debit of its origin and a credit of its
// destination, each within its own product's limits; when one is exceeded, nothing moves.
func TestATransferStaysWithinBothAccountsLimits(t *testing.T) {
	w := newWorld(t)
	w.minimumWage(t)
	w.vault(t)
	from, to := w.limited(t, `{"per_operation": {"units": 1000}}`), w.simplified(t)
	steps(t,
		func() error { return w.deposit(t, from.ID, 1000) },
		func() error { return w.deposit(t, from.ID, 1000) },
		func() error { return w.deposit(t, to.ID, threeSMMLV-500) },
	)

	expectExceeds(t, "over the origin's operation", w.transfer(t, from.ID, to.ID, 1001), "per_operation", 1000)
	expectExceeds(t, "over the destination's balance", w.transfer(t, from.ID, to.ID, 600), "max_balance", threeSMMLV)
	if err := w.transfer(t, from.ID, to.ID, 500); err != nil {
		t.Fatal(err)
	}

	if w.balance(t, from) != 1500 || w.balance(t, to) != threeSMMLV {
		t.Fatalf("balances = %d and %d, want 1500 and %d", w.balance(t, from), w.balance(t, to), threeSMMLV)
	}
	if got := counters(t, from.ID)["2026-03-02"]; got != (moved{500, 2000}) {
		t.Fatalf("counters of the origin = %v, want 500 debited and 2000 credited", got)
	}
	if got := counters(t, to.ID)["2026-03-02"]; got != (moved{0, threeSMMLV}) {
		t.Fatalf("counters of the destination = %v, want %d credited", got, threeSMMLV)
	}
}

// TestWithoutAMinimumWageNothingMoves: a product with limits in SMMLV does not move money on a day without a
// minimum wage in force (fail closed); one without them does not need it.
func TestWithoutAMinimumWageNothingMoves(t *testing.T) {
	w := newWorldOn(t, "2031-03-03")
	w.vault(t)
	simplified, savings := w.simplified(t), w.active(t)

	err := w.deposit(t, simplified.ID, 100)

	if !errors.Is(err, deposits.ErrMinimumWageMissing) || w.balance(t, simplified) != 0 {
		t.Fatalf("deposit without a minimum wage = %v, balance %d; want ErrMinimumWageMissing and 0", err, w.balance(t, simplified))
	}
	if err := w.deposit(t, savings.ID, 100); err != nil {
		t.Fatalf("deposit into a product without limits in SMMLV = %v", err)
	}
}

// TestConcurrentDepositsDoNotPassTheMaximum (FR-24): a deposit that comes while another is in progress waits
// for it under the account's lock and then sees its balance: two deposits that fit alone but not together
// do not both go in.
func TestConcurrentDepositsDoNotPassTheMaximum(t *testing.T) {
	w := newWorld(t)
	w.minimumWage(t)
	w.vault(t)
	a := w.simplified(t)
	deposit := func(units int64) func(tx postgres.Tx) error {
		return func(tx postgres.Tx) error {
			_, err := w.deposits.Deposit(t.Context(), tx, w.cashIn(units, a.ID, copOf(t)))
			return err
		}
	}

	late := race(t, deposit(threeSMMLV), deposit(100))

	expectExceeds(t, "the deposit that waited", late, "max_balance", threeSMMLV)
	if w.balance(t, a) != threeSMMLV {
		t.Fatalf("balance = %d, want %d", w.balance(t, a), threeSMMLV)
	}
}

// TestTheDatabaseGuardsTheCounters: even as the tables' owner, a counter does not go down, change its account
// or period, or get deleted or truncated.
func TestTheDatabaseGuardsTheCounters(t *testing.T) {
	w := newWorld(t)
	w.vault(t)
	a := w.active(t)
	if err := w.deposit(t, a.ID, 1000); err != nil {
		t.Fatal(err)
	}
	where := ` WHERE account_id = '` + a.ID.String() + `'`
	expectProhibited(t, []prohibited{
		{"lower the credits", `UPDATE deposits.counters SET credits = credits - 1` + where, "only grow"},
		{"move to another period", `UPDATE deposits.counters SET period = '2020-01'` + where + ` AND period = '2026-03'`, "only grow"},
		{"delete", `DELETE FROM deposits.counters` + where, "never deleted or truncated"},
		{"truncate", `TRUNCATE deposits.counters`, "never deleted or truncated"},
	})
}

// TestTheLedgerRejectsFirst: the limits are checked after the ledger accepts the posting, so its rejections
// come first. A movement whose reference was already posted is a duplicate, also when it would now exceed a
// limit (the retry of a deposit that reached the maximum); one in another currency is a currency mismatch,
// not a limit in units of another currency.
func TestTheLedgerRejectsFirst(t *testing.T) {
	w := newWorld(t)
	w.minimumWage(t)
	w.vault(t)
	simplified, limited := w.simplified(t), w.limited(t, `{"per_operation": {"units": 1000}}`)
	full := w.cashIn(threeSMMLV, simplified.ID, copOf(t))
	if err := w.depositOf(t, full); err != nil {
		t.Fatal(err)
	}
	usd, err := money.CurrencyOf("USD")
	if err != nil {
		t.Fatal(err)
	}

	again := w.depositOf(t, full)
	inDollars := w.depositOf(t, w.cashIn(1001, limited.ID, usd))

	if !errors.Is(again, ledger.ErrDuplicateExternalRef) || !errors.Is(inDollars, ledger.ErrCurrencyMismatch) {
		t.Fatalf("the reference again = %v, in dollars = %v; want ErrDuplicateExternalRef and ErrCurrencyMismatch", again, inDollars)
	}
	if got := counters(t, simplified.ID)["2026-03"]; got != (moved{0, threeSMMLV}) || w.balance(t, simplified) != threeSMMLV {
		t.Fatalf("counters %v and balance %d, want the first deposit only", got, w.balance(t, simplified))
	}
}

// TestAnSMMLVLimitOutsidePesosMovesNothing: a product in dollars with a limit in SMMLV cannot convert it, so
// its accounts do not move money (a misconfigured product, ErrSMMLVOutsideCOP).
func TestAnSMMLVLimitOutsidePesosMovesNothing(t *testing.T) {
	w := newWorld(t)
	w.minimumWage(t)
	usd, err := money.CurrencyOf("USD")
	if err != nil {
		t.Fatal(err)
	}
	w.vaultIn(t, usd)
	a := w.limitedIn(t, "USD", `{"max_balance": {"smmlv": 3}}`)

	err = w.depositOf(t, w.cashIn(100, a.ID, usd))

	if !errors.Is(err, deposits.ErrSMMLVOutsideCOP) || w.balance(t, a) != 0 {
		t.Fatalf("deposit = %v with a balance of %d, want ErrSMMLVOutsideCOP and 0", err, w.balance(t, a))
	}
}

// TestARejectedMovementLeavesNothing: a deposit over a limit is rejected after the ledger accepted its
// posting, but the facade undoes it, so a caller that commits its transaction anyway commits nothing of it:
// no balance, no counters, no ledger transaction.
func TestARejectedMovementLeavesNothing(t *testing.T) {
	w := newWorld(t)
	w.vault(t)
	a := w.limited(t, `{"per_operation": {"units": 1000}}`)
	in := w.cashIn(1001, a.ID, copOf(t))
	var rejected error

	inTx(t, func(tx postgres.Tx) error {
		_, rejected = w.deposits.Deposit(t.Context(), tx, in)
		return nil // commits whatever the deposit left
	})

	var postings int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM ledger.transactions WHERE source = 'deposits' AND external_ref = $1`,
		in.ExternalRef).Scan(&postings); err != nil {
		t.Fatal(err)
	}
	expectExceeds(t, "the deposit", rejected, "per_operation", 1000)
	if w.balance(t, a) != 0 || len(counters(t, a.ID)) != 0 || postings != 0 {
		t.Fatalf("after committing: balance %d, counters %v, %d postings; want nothing", w.balance(t, a), counters(t, a.ID), postings)
	}
}
