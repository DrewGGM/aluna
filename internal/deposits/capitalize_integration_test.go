//go:build integration

package deposits_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// expense is the interest expense account the savings product names (E.INTEREST.SAVINGS), which vault
// creates.
func (w world) expense(t *testing.T) ids.ID {
	t.Helper()
	a, err := w.ledger.AccountByCode(t.Context(), testPool, w.entity, "E.INTEREST.SAVINGS")
	if err != nil {
		t.Fatal(err)
	}
	return a.ID
}

// rollUntil rolls the world's business date over until it is after day.
func (w world) rollUntil(t *testing.T, day calendar.Date) {
	t.Helper()
	cal := calendar.New()
	for {
		var today calendar.Date
		inTx(t, func(tx postgres.Tx) error {
			var err error
			today, err = cal.BusinessDate(t.Context(), tx, w.entity)
			return err
		})
		if time.Date(today.Year, today.Month, today.Day, 0, 0, 0, 0, time.UTC).After(time.Date(day.Year, day.Month, day.Day, 0, 0, 0, 0, time.UTC)) {
			return
		}
		w.rollover(t, today)
	}
}

// capitalization is the account's capitalization of period, or false if there is none.
func (w world) capitalization(t *testing.T, a deposits.Account, period string) (ledger.Transaction, bool) {
	t.Helper()
	var id ids.ID
	err := testPool.QueryRow(t.Context(), `SELECT id FROM ledger.transactions WHERE legal_entity_id = $1 AND source = 'deposits.capitalization'
		AND external_ref = $2`, w.entity, a.ID.String()+":"+period).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ledger.Transaction{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	posted, err := w.ledger.Transaction(t.Context(), testPool, w.entity, id)
	if err != nil {
		t.Fatal(err)
	}
	return posted, true
}

// exponential is the interest of balance units over days at effective annual rate ea under ACT/365F, by the
// exponential formula, rounded half even to minor units of COP: balance × ((1 + ea)^(days/365) − 1).
func exponential(t *testing.T, balance int64, ea string, days int) int64 {
	t.Helper()
	must := func(p money.Precise, err error) money.Precise {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	cop := copOf(t)
	fraction := must(must(money.ParsePrecise(strconv.Itoa(days))).Quo(must(money.ParsePrecise("365")), 34, money.HalfEven))
	growth := must(must(money.ParsePrecise("1")).Add(must(money.ParsePrecise(ea))))
	grown := must(money.New(balance, cop).Precise().Mul(must(growth.Pow(fraction))))
	earned := must(grown.Add(money.New(-balance, cop).Precise()))
	amount, _, err := earned.ToAmount(cop, money.HalfEven)
	if err != nil {
		t.Fatal(err)
	}
	return amount.Units()
}

// TestAMonthOfInterestMatchesTheExponentialFormula (US-6, FR-26): at 6 % EA on a constant balance of 1,000,000
// COP through April, the capitalization on April 30 posts, from the product's interest expense into the
// account, the exponential formula to the cent, once, and emits aluna.deposits.interest.capitalized.v1; the
// days after accrue on the new balance.
func TestAMonthOfInterestMatchesTheExponentialFormula(t *testing.T) {
	w := newWorldOn(t, "2026-04-01")
	w.vault(t)
	expense := w.expense(t)
	a := w.openWith(t, map[string]string{"rate_tiers": `[{"from_units": 0, "rate_ea": "0.06"}]`})
	const balance = 100000000
	if err := w.deposit(t, a.ID, balance); err != nil {
		t.Fatal(err)
	}
	w.rollUntil(t, calendar.Date{Year: 2026, Month: time.April, Day: 30})

	w.accrue(t)
	w.accrue(t)

	want := exponential(t, balance, "0.06", 30)
	if want != 480071 { // 4,800.71 COP, also computed apart with 50 digits
		t.Fatalf("the formula gives %d units, want 480071", want)
	}
	april30 := calendar.Date{Year: 2026, Month: time.April, Day: 30}
	posted := w.expectCapitalization(t, a, expense, "2026-04", april30, want)
	if w.balance(t, a) != balance+want {
		t.Fatalf("balance = %d, want %d", w.balance(t, a), balance+want)
	}
	w.expectCapitalized(t, a, posted, want)
	// rows[29] is April 30; the rollover from April 30 goes to Monday May 4 (May 1 is a holiday), so May 1 to 3
	// are accrued too
	rows := storedAccruals(t, a.ID)
	if len(rows) != 33 {
		t.Fatalf("%d days accrued, want April 1 to May 3 (33)", len(rows))
	}
	if april := rows[29]; april.ValueDate != april30 || april.PostedUnits != want {
		t.Fatalf("accrual of April 30 = %+v, want it with %d posted", april, want)
	}
	if may := rows[30]; may.BalanceUnits != balance+want || may.PostedUnits != want {
		t.Fatalf("accrual of May 1 = %+v, want it on %d with %d posted", may, balance+want, want)
	}
}

// expectCapitalization checks that a has a capitalization of period, of units from expense, on value date on,
// by the deposit rules and rounded half even, and returns it.
func (w world) expectCapitalization(t *testing.T, a deposits.Account, expense ids.ID, period string, on calendar.Date, units int64) ledger.Transaction {
	t.Helper()
	posted, ok := w.capitalization(t, a, period)
	if !ok {
		t.Fatalf("no capitalization of %s", period)
	}
	cop := copOf(t)
	want := []ledger.EntryInput{{AccountID: expense, Amount: money.New(units, cop)}, {AccountID: a.LedgerAccountID, Amount: money.New(-units, cop)}}
	if !slices.Equal(posted.Entries, want) {
		t.Fatalf("capitalization entries = %+v, want %+v", posted.Entries, want)
	}
	header := posted.Code == "deposits.interest_capitalization" && posted.RuleVersion == "deposits/v1" &&
		posted.Metadata["rounding"] == "half_even" && posted.Actor == "deposits.accrual" && posted.Channel == "job"
	if !header || !posted.ValueDate.Equal(time.Date(on.Year, on.Month, on.Day, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("capitalization = %+v, want the deposit rules, half even, by the accrual job, on %v", posted, on)
	}
	return posted
}

// expectCapitalized checks that the world's events hold, once, the capitalization of a for April 2026.
func (w world) expectCapitalized(t *testing.T, a deposits.Account, posted ledger.Transaction, units int64) {
	t.Helper()
	var found []string
	for _, e := range w.events.events {
		if e.Type != "aluna.deposits.interest.capitalized.v1" {
			continue
		}
		data, err := json.Marshal(e.Data)
		if err != nil {
			t.Fatal(err)
		}
		found = append(found, string(data))
		if e.Subject != "deposit-accounts/"+a.ID.String() || e.LegalEntityID != w.entity || e.SchemaVersion != 1 {
			t.Fatalf("event = %+v, want it about the account", e)
		}
	}
	want := `{"account_id":"` + a.ID.String() + `","period":"2026-04","transaction_id":"` + posted.ID.String() +
		`","value_date":"2026-04-30","amount":` + strconv.FormatInt(units, 10) + `,"currency":"COP"}`
	if len(found) != 1 || found[0] != want {
		t.Fatalf("capitalized events = %v, want once %s", found, want)
	}
}

// TestNothingToCapitalizePostsNothing: an account that earned nothing in the period posts no capitalization
// (the posting rules take no zero amount) and still accrues its days.
func TestNothingToCapitalizePostsNothing(t *testing.T) {
	w := newWorldOn(t, "2026-04-01")
	w.vault(t)
	a := w.active(t)
	w.rollUntil(t, calendar.Date{Year: 2026, Month: time.April, Day: 30})

	w.accrue(t)

	if _, ok := w.capitalization(t, a, "2026-04"); ok {
		t.Fatal("an empty account capitalized April")
	}
	if days := len(storedAccruals(t, a.ID)); days < 30 {
		t.Fatalf("%d days accrued, want every day of April", days)
	}
}

// TestAMovedCapitalizationDateCapitalizesItsPeriod (FR-26): the savings product capitalizes on the following
// business day; February 2026 ends on Saturday the 28th, so Monday March 2 capitalizes February, with the
// interest of that day (the account opened on it), on value date March 2.
func TestAMovedCapitalizationDateCapitalizesItsPeriod(t *testing.T) {
	w := newWorld(t)
	w.vault(t)
	a := w.active(t)
	if err := w.deposit(t, a.ID, 100000000); err != nil {
		t.Fatal(err)
	}
	w.rollover(t, march(2))

	w.accrue(t)

	rows := storedAccruals(t, a.ID)
	if len(rows) != 1 || rows[0].PostedUnits == 0 {
		t.Fatalf("accruals = %+v, want March 2 with its interest posted", rows)
	}
	w.expectCapitalization(t, a, w.expense(t), "2026-02", march(2), rows[0].PostedUnits)
	if w.balance(t, a) != 100000000+rows[0].PostedUnits {
		t.Fatalf("balance = %d, want %d", w.balance(t, a), 100000000+rows[0].PostedUnits)
	}
}

// TestAMonthEndBatchFitsTheLockTimeout (FR-26): on a capitalization day every account also posts and emits, so
// a batch of the worker's job (50 accounts, accrualBatch in internal/jobs/jobs.go) must still end well before
// the pool's lock_timeout (2 s), which a movement waiting on one of its accounts would hit. It measures the
// average batch, with a margin of half the timeout.
func TestAMonthEndBatchFitsTheLockTimeout(t *testing.T) {
	w := newWorld(t)
	vault := w.vault(t)
	sample := w.active(t) // following: Monday March 2 capitalizes February
	const n, batch = 1000, 50
	w.fund(t, vault, w.insertAccounts(t, sample.PartyID, sample.ProductVersionID, n), 1000000)
	w.rollover(t, march(2))

	start := time.Now()
	w.accrueIn(t, batch)
	perBatch := time.Since(start) / (n / batch)

	var capitalized int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM ledger.transactions WHERE legal_entity_id = $1
		AND source = 'deposits.capitalization'`, w.entity).Scan(&capitalized); err != nil {
		t.Fatal(err)
	}
	if capitalized != n || perBatch >= time.Second {
		t.Fatalf("%d capitalizations, %s per batch of %d; want %d, under a second", capitalized, perBatch, batch, n)
	}
	t.Logf("a capitalizing batch of %d accounts took %s", batch, perBatch)
}
