package interest

import (
	"fmt"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/products"
)

func cop(t *testing.T) money.Currency {
	t.Helper()
	c, err := money.CurrencyOf("COP")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func tier(t *testing.T, from int64, ea string) products.RateTier {
	t.Helper()
	return products.RateTier{FromUnits: from, RateEA: dec(t, ea)}
}

// accrue accrues day d under ACT/365F.
func accrue(t *testing.T, state AccrualState, balance int64, tiers []products.RateTier, d calendar.Date) (AccrualState, money.Precise) {
	t.Helper()
	next, interest, err := AccrueDay(state, balance, tiers, products.ACT365F, d)
	if err != nil {
		t.Fatalf("AccrueDay(%d, %v): %v", balance, d, err)
	}
	return next, interest
}

// rateOfADay is the rate of one day under ACT/365F.
func rateOfADay(t *testing.T, ea string) money.Precise {
	t.Helper()
	r, err := rateForFraction(dec(t, ea), ratio(t, "1", "365"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func product(t *testing.T, a, b money.Precise) money.Precise {
	t.Helper()
	p, err := a.Mul(b)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// accrualTolerance is the accrual's rounding of each day's interest (accrualScale decimals of a minor unit).
const accrualTolerance = "0.00000000000000000001"

// TestTiersApplyBySlab: each tier's rate applies to the part of the balance in its slab; a balance below
// the second tier, or exactly on its start, earns only the first rate, and an empty or negative balance
// earns nothing.
func TestTiersApplyBySlab(t *testing.T) {
	tiers := []products.RateTier{tier(t, 0, "0.01"), tier(t, 1000000, "0.05")}
	jan1 := day(2026, time.January, 1)

	_, slabs := accrue(t, AccrualState{}, 1500000, tiers, jan1)
	_, first := accrue(t, AccrualState{}, 400000, tiers, jan1)
	_, empty := accrue(t, AccrualState{}, 0, tiers, jan1)
	_, negative := accrue(t, AccrualState{}, -50000, tiers, jan1)
	_, onStart := accrue(t, AccrualState{}, 1000000, tiers, jan1)

	want := sum(t, product(t, dec(t, "1000000"), rateOfADay(t, "0.01")), product(t, dec(t, "500000"), rateOfADay(t, "0.05")))
	if !near(t, slabs, want.String(), accrualTolerance) {
		t.Errorf("1,500,000 over two slabs = %s, want %s", slabs, want)
	}
	if want := product(t, dec(t, "400000"), rateOfADay(t, "0.01")); !near(t, first, want.String(), accrualTolerance) {
		t.Errorf("400,000 in the first slab = %s, want %s", first, want)
	}
	if want := product(t, dec(t, "1000000"), rateOfADay(t, "0.01")); !near(t, onStart, want.String(), accrualTolerance) {
		t.Errorf("1,000,000, on the second tier's start = %s, want all of it at the first rate %s", onStart, want)
	}
	if empty.Cmp(money.Precise{}) != 0 || negative.Cmp(money.Precise{}) != 0 {
		t.Errorf("empty balance = %s, negative balance = %s; want 0", empty, negative)
	}
}

// TestUnpostedInterestEarnsInterest: interest accrued and not posted yet compounds with the balance, so the
// second day earns on the balance plus the first day's interest; with no balance it still earns, and only a
// base (balance plus unposted interest) of zero or less earns nothing.
func TestUnpostedInterestEarnsInterest(t *testing.T) {
	tiers := []products.RateTier{tier(t, 0, "0.06")}
	state, first := accrue(t, AccrualState{}, 10000000, tiers, day(2026, time.March, 1))
	_, second := accrue(t, state, 10000000, tiers, day(2026, time.March, 2))

	want := product(t, sum(t, dec(t, "10000000"), first), rateOfADay(t, "0.06"))
	if !near(t, second, want.String(), accrualTolerance) {
		t.Fatalf("second day = %s, want (balance + first day %s) × rate = %s", second, first, want)
	}
	if state.AccruedPrecise.Cmp(first) != 0 {
		t.Fatalf("accrued after a day = %s, want the day's interest %s", state.AccruedPrecise, first)
	}
	unposted := AccrualState{AccruedPrecise: dec(t, "5000")}
	_, onlyUnposted := accrue(t, unposted, 0, tiers, day(2026, time.March, 3))
	_, below := accrue(t, unposted, -5000, tiers, day(2026, time.March, 3))
	if want := product(t, dec(t, "5000"), rateOfADay(t, "0.06")); !near(t, onlyUnposted, want.String(), accrualTolerance) {
		t.Errorf("no balance and 5,000 unposted = %s, want %s", onlyUnposted, want)
	}
	if below.Cmp(money.Precise{}) != 0 {
		t.Errorf("a base of zero (−5,000 + 5,000 unposted) = %s, want 0", below)
	}
}

// TestPostedInterestEarnsOnce: once posted, the interest is in the balance (capitalized) and earns as part
// of it, not a second time as accrued: the next day earns on the new balance plus only the residue.
func TestPostedInterestEarnsOnce(t *testing.T) {
	tiers := []products.RateTier{tier(t, 0, "0.30")}
	state, _ := accrue(t, AccrualState{}, 100000000000, tiers, day(2026, time.March, 1))
	posted, state, err := DueForPosting(state, cop(t), money.HalfEven)
	if err != nil {
		t.Fatal(err)
	}
	residue := sum(t, state.AccruedPrecise, wholeT(t, -state.PostedUnits))

	_, next := accrue(t, state, 100000000000+posted, tiers, day(2026, time.March, 2))

	want := product(t, sum(t, wholeT(t, 100000000000+posted), residue), rateOfADay(t, "0.30"))
	if !near(t, next, want.String(), accrualTolerance) {
		t.Fatalf("day after posting %d = %s, want (capitalized balance + residue %s) × rate = %s", posted, next, residue, want)
	}
}

// TestPostingRoundsAndKeepsTheResidue: posting rounds the accrued interest to minor units with the
// product's mode and keeps the rest for later; posting again without new interest posts nothing.
func TestPostingRoundsAndKeepsTheResidue(t *testing.T) {
	state := AccrualState{AccruedPrecise: dec(t, "1234.56")} // minor units: 12.3456 COP
	units, next, err := DueForPosting(state, cop(t), money.HalfEven)
	if err != nil {
		t.Fatal(err)
	}
	again, after, err := DueForPosting(next, cop(t), money.HalfEven)
	if err != nil {
		t.Fatal(err)
	}
	if units != 1235 || next.PostedUnits != 1235 || again != 0 || after.PostedUnits != 1235 {
		t.Fatalf("posted %d (state %+v), then %d (state %+v); want 1235 and then 0", units, next, again, after)
	}
	if down, _, err := DueForPosting(state, cop(t), money.Down); err != nil || down != 1234 {
		t.Fatalf("posting with Down = %d (%v), want 1234", down, err)
	}
}

// roundedUnits is the accrual rounded to whole minor units with mode, as posting rounds it.
func roundedUnits(rt *rapid.T, accrued money.Precise, mode money.RoundingMode) money.Precise {
	r, err := accrued.Quo(whole(rt, 1), 0, mode)
	if err != nil {
		rt.Fatal(err)
	}
	return r
}

func whole(rt *rapid.T, n int64) money.Precise {
	p, err := precise64(n)
	if err != nil {
		rt.Fatal(err)
	}
	return p
}

func wholeT(t *testing.T, n int64) money.Precise {
	t.Helper()
	p, err := precise64(n)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// drawEA draws an effective annual rate from 0 to 30 % with up to 4 decimals.
func drawEA(rt *rapid.T, label string, minimum int) money.Precise {
	ea, err := money.ParsePrecise(fmt.Sprintf("0.%04d", rapid.IntRange(minimum, 3000).Draw(rt, label)))
	if err != nil {
		rt.Fatal(err)
	}
	return ea
}

// genTiers draws one to four tiers from 0 with rising starts and rates from 0 to 30 % EA.
func genTiers(rt *rapid.T) []products.RateTier {
	n := rapid.IntRange(1, 4).Draw(rt, "tiers")
	tiers := make([]products.RateTier, n)
	var from int64
	for i := range tiers {
		if i > 0 {
			from += rapid.Int64Range(1, 100000000000).Draw(rt, "slab")
		}
		tiers[i] = products.RateTier{FromUnits: from, RateEA: drawEA(rt, "ea", 0)}
	}
	return tiers
}

// TestPostingNeverOutrunsTheAccrual (property): over any days of any balances and tiers, with postings on
// any days and any rounding mode, what each posting returns is never negative and adds up to what is
// posted, what is posted never exceeds the accrual rounded with that mode, and right after a posting the
// residue (accrued less posted) is below one minor unit.
func TestPostingNeverOutrunsTheAccrual(t *testing.T) {
	c := cop(t)
	rapid.Check(t, func(rt *rapid.T) {
		tiers := genTiers(rt)
		dc := rapid.SampledFrom([]products.DayCount{products.ACT365F, products.ACT360, products.E30360, products.ACTACT}).Draw(rt, "dc")
		mode := rapid.SampledFrom([]money.RoundingMode{money.HalfEven, money.HalfUp, money.Down, money.Up}).Draw(rt, "mode")
		year, month := rapid.IntRange(2024, 2030).Draw(rt, "year"), rapid.IntRange(1, 12).Draw(rt, "month")
		d := day(year, time.Month(month), rapid.IntRange(1, 28).Draw(rt, "day"))
		var state AccrualState
		var dues int64
		for range rapid.IntRange(1, 40).Draw(rt, "days") {
			balance := rapid.Int64Range(-1000000, 1000000000000).Draw(rt, "balance")
			var err error
			if state, _, err = AccrueDay(state, balance, tiers, dc, d); err != nil {
				rt.Fatal(err)
			}
			if rapid.Bool().Draw(rt, "post") {
				dues += post(rt, &state, c, mode)
			}
			if dues != state.PostedUnits || whole(rt, state.PostedUnits).Cmp(roundedUnits(rt, state.AccruedPrecise, mode)) > 0 {
				rt.Fatalf("posted %d in total, state %d, accrual %s", dues, state.PostedUnits, state.AccruedPrecise)
			}
			d = nextDay(d)
		}
	})
}

// post posts state, checks that what is due is not negative and that the residue is below one minor unit,
// and returns what was due.
func post(rt *rapid.T, state *AccrualState, c money.Currency, mode money.RoundingMode) int64 {
	due, next, err := DueForPosting(*state, c, mode)
	if err != nil {
		rt.Fatal(err)
	}
	residue, err := minus(next.AccruedPrecise, whole(rt, next.PostedUnits))
	if err != nil {
		rt.Fatal(err)
	}
	if due < 0 || residue.Cmp(whole(rt, 1)) >= 0 || residue.Cmp(whole(rt, -1)) <= 0 {
		rt.Fatalf("posting returned %d with residue %s, want it not negative and the residue below a minor unit", due, residue)
	}
	*state = next
	return due
}

// TestAYearOfAccrualIsTheEA (property, US-6): a constant balance accrued every day of a calendar year and
// then posted earns balance × EA to the cent, under the conventions whose year adds up to 1 (ACT/365F,
// 30E/360 and ACT/ACT; under ACT/360 a year of 365 days is 365/360 of a year by definition).
func TestAYearOfAccrualIsTheEA(t *testing.T) {
	// half a minor unit, the most rounding to the cent may give, plus the error of about 366 days rounded to
	// accrualScale decimals and of Pow, far below 1e-10
	c, half := cop(t), dec(t, "0.5000000001")
	rapid.Check(t, func(rt *rapid.T) {
		balance, ea := rapid.Int64Range(1, 1000000000000).Draw(rt, "balance"), drawEA(rt, "ea", 1)
		dc := rapid.SampledFrom([]products.DayCount{products.ACT365F, products.E30360, products.ACTACT}).Draw(rt, "dc")
		years := []int{2026, 2027, 2028}
		if dc == products.ACT365F {
			years = []int{2026, 2027} // a leap year is 366/365 of a year under ACT/365F
		}
		year := rapid.SampledFrom(years).Draw(rt, "year")
		posted := yearOfAccrual(rt, c, balance, ea, dc, year)
		exact, err := whole(rt, balance).Mul(ea)
		if err != nil {
			rt.Fatal(err)
		}
		if gap := distance(rt, exact, whole(rt, posted)); gap.Cmp(half) > 0 {
			rt.Fatalf("a year of %d at %s EA under %s posted %d, want %s to the cent", balance, ea, dc, posted, exact)
		}
	})
}

// yearOfAccrual accrues balance every day of year and posts it, half even.
func yearOfAccrual(rt *rapid.T, c money.Currency, balance int64, ea money.Precise, dc products.DayCount, year int) int64 {
	tiers := []products.RateTier{{FromUnits: 0, RateEA: ea}}
	var state AccrualState
	for d := day(year, time.January, 1); d.Year == year; d = nextDay(d) {
		var err error
		if state, _, err = AccrueDay(state, balance, tiers, dc, d); err != nil {
			rt.Fatal(err)
		}
	}
	posted, _, err := DueForPosting(state, c, money.HalfEven)
	if err != nil {
		rt.Fatal(err)
	}
	return posted
}

// distance is |a − b|.
func distance(rt *rapid.T, a, b money.Precise) money.Precise {
	d, err := minus(a, b)
	if err != nil {
		rt.Fatal(err)
	}
	if d.Cmp(money.Precise{}) < 0 {
		if d, err = minus(money.Precise{}, d); err != nil {
			rt.Fatal(err)
		}
	}
	return d
}
