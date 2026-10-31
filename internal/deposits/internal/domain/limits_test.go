package domain

import (
	"errors"
	"math"
	"math/big"
	"testing"

	"pgregory.net/rapid"

	"github.com/DrewGGM/aluna/internal/money"
)

func currencyOf(t *testing.T, code string) money.Currency {
	t.Helper()
	c, err := money.CurrencyOf(code)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func preciseOf(t *testing.T, s string) money.Precise {
	t.Helper()
	p, err := money.ParsePrecise(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLimitsConvertToMinorUnits (FR-24): a limit in units stays; one in SMMLV is that many minimum wages of the
// day in COP minor units; an absent one does not apply.
func TestLimitsConvertToMinorUnits(t *testing.T) {
	l := Limits{PerOperation: Limit{Units: 500000}, MonthlyDebits: Limit{SMMLV: 3}, MaxBalance: Limit{SMMLV: 3}}

	caps, err := l.InUnits(currencyOf(t, "COP"), preciseOf(t, "1423500"))
	if err != nil {
		t.Fatal(err)
	}

	want := Caps{PerOperation: 500000, MonthlyDebits: 427050000, MaxBalance: 427050000}
	if caps != want {
		t.Fatalf("caps = %+v, want %+v", caps, want)
	}
}

// TestAnSMMLVLimitIsInPesos: the minimum wage is in COP, so a limit in SMMLV does not convert to another
// currency; a limit in units does.
func TestAnSMMLVLimitIsInPesos(t *testing.T) {
	usd := currencyOf(t, "USD")
	wage := preciseOf(t, "1423500")

	_, inSMMLV := Limits{MaxBalance: Limit{SMMLV: 3}}.InUnits(usd, wage)
	inUnits, err := Limits{MaxBalance: Limit{Units: 100}}.InUnits(usd, wage)

	if !errors.Is(inSMMLV, ErrSMMLVOutsideCOP) || err != nil || inUnits.MaxBalance != 100 {
		t.Fatalf("SMMLV in USD = %v; units in USD = %+v (%v); want ErrSMMLVOutsideCOP and a cap of 100", inSMMLV, inUnits, err)
	}
}

// TestUsesSMMLV: only a limit in SMMLV, whichever it is, needs the market parameter.
func TestUsesSMMLV(t *testing.T) {
	inSMMLV := Limit{SMMLV: 1}
	for _, l := range []Limits{{PerOperation: inSMMLV}, {DailyDebits: inSMMLV}, {MonthlyDebits: inSMMLV}, {MaxBalance: inSMMLV}} {
		if !l.UsesSMMLV() {
			t.Errorf("%+v does not use SMMLV, want it to", l)
		}
	}
	if (Limits{PerOperation: Limit{Units: 1}, MaxBalance: Limit{Units: 1}}).UsesSMMLV() || (Limits{}).UsesSMMLV() {
		t.Fatal("limits in units use SMMLV, want them not to")
	}
}

// TestADebitStaysWithinItsLimits (FR-24): a debit may reach each limit exactly; one unit more is
// ErrLimitExceeded naming the limit it exceeds.
func TestADebitStaysWithinItsLimits(t *testing.T) {
	caps := Caps{PerOperation: 1000, DailyDebits: 1500, MonthlyDebits: 3000, MaxBalance: 10} // a debit does not look at the balance
	for name, c := range map[string]struct {
		used     Usage
		units    int64
		exceeded string
	}{
		"up to every limit":      {Usage{DailyDebits: 500, MonthlyDebits: 2000, Balance: 5000}, 1000, ""},
		"over the operation":     {Usage{}, 1001, "per_operation"},
		"over the day":           {Usage{DailyDebits: 501, MonthlyDebits: 501}, 1000, "daily_debits"},
		"over the month":         {Usage{DailyDebits: 0, MonthlyDebits: 2001}, 1000, "monthly_debits"},
		"already over the month": {Usage{MonthlyDebits: 3500}, 1, "monthly_debits"},
	} {
		expectLimit(t, name, caps.CheckDebit(c.used, c.units), c.exceeded, caps)
	}
}

// TestACreditStaysWithinItsLimits (FR-24, PRD edge case): a credit may bring the balance exactly to the
// maximum; one that exceeds it is rejected whole, not credited in part.
func TestACreditStaysWithinItsLimits(t *testing.T) {
	caps := Caps{PerOperation: 1000, DailyDebits: 1, MaxBalance: 3000} // a credit does not look at the debits
	for name, c := range map[string]struct {
		used     Usage
		units    int64
		exceeded string
	}{
		"up to the maximum balance": {Usage{DailyDebits: 900, Balance: 2000}, 1000, ""},
		"over the operation":        {Usage{}, 1001, "per_operation"},
		"over the maximum balance":  {Usage{Balance: 2001}, 1000, "max_balance"},
	} {
		expectLimit(t, name, caps.CheckCredit(c.used, c.units), c.exceeded, caps)
	}
}

// TestWithoutLimitsAnythingGoes: limits that do not apply accept any amount.
func TestWithoutLimitsAnythingGoes(t *testing.T) {
	if !(Caps{}).Unlimited() || (Caps{MaxBalance: 1}).Unlimited() {
		t.Fatal("Unlimited must be true only without caps")
	}
	full := Usage{DailyDebits: math.MaxInt64, MonthlyDebits: math.MaxInt64, Balance: math.MaxInt64}
	if err := (Caps{}).CheckDebit(full, math.MaxInt64); err != nil {
		t.Fatalf("debit without limits = %v", err)
	}
	if err := (Caps{}).CheckCredit(full, math.MaxInt64); err != nil {
		t.Fatalf("credit without limits = %v", err)
	}
}

// TestAWageBelowOneUnitIsRejected: a minimum wage so low that a limit in SMMLV rounds to less than one minor
// unit (the database only asks it to be positive) is a wrong market parameter, not a limit of 0 that would
// not apply: it fails closed.
func TestAWageBelowOneUnitIsRejected(t *testing.T) {
	_, err := Limits{MaxBalance: Limit{SMMLV: 3}}.InUnits(currencyOf(t, "COP"), preciseOf(t, "0.001"))

	if !errors.Is(err, ErrMinimumWageTooLow) {
		t.Fatalf("a wage of 0.001 pesos = %v, want ErrMinimumWageTooLow", err)
	}
}

// expectLimit checks that err exceeds the limit named exceeded of caps (nil if empty).
func expectLimit(t *testing.T, name string, err error, exceeded string, caps Caps) {
	t.Helper()
	if exceeded == "" {
		if err != nil {
			t.Errorf("%s = %v, want it accepted", name, err)
		}
		return
	}
	var limit *LimitError
	if !errors.Is(err, ErrLimitExceeded) || !errors.As(err, &limit) || limit.Name != exceeded || limit.Units != caps.named(exceeded) {
		t.Errorf("%s = %v, want ErrLimitExceeded of %s", name, err, exceeded)
	}
}

func (c Caps) named(name string) int64 {
	return map[string]int64{
		limitPerOperation: c.PerOperation, limitDailyDebits: c.DailyDebits, limitMonthlyDebits: c.MonthlyDebits, limitMaxBalance: c.MaxBalance,
	}[name]
}

// TestACapAcceptsExactlyWhatFits (property): for any cap (at least one unit), use so far and amount, a debit
// is accepted if and only if use + amount ≤ cap, computed without overflow.
func TestACapAcceptsExactlyWhatFits(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		limit := rapid.Int64Range(1, math.MaxInt64).Draw(rt, "cap")
		used := rapid.Int64Range(0, math.MaxInt64).Draw(rt, "used")
		units := rapid.Int64Range(1, math.MaxInt64).Draw(rt, "units")
		caps := Caps{DailyDebits: limit}

		err := caps.CheckDebit(Usage{DailyDebits: used}, units)

		fits := new(big.Int).Add(big.NewInt(used), big.NewInt(units)).Cmp(big.NewInt(limit)) <= 0
		if fits != (err == nil) {
			rt.Fatalf("cap %d, used %d, amount %d: error %v, want it accepted: %v", limit, used, units, err, fits)
		}
	})
}
