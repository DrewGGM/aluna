package interest

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/products"
)

func dec(t *testing.T, s string) money.Precise {
	t.Helper()
	p, err := money.ParsePrecise(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// ratio is num/den at fractionScale, as YearFraction rounds a fraction.
func ratio(t *testing.T, num, den string) money.Precise {
	t.Helper()
	r, err := dec(t, num).Quo(dec(t, den), fractionScale, money.HalfEven)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func sum(t *testing.T, a, b money.Precise) money.Precise {
	t.Helper()
	s, err := a.Add(b)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func day(y int, m time.Month, d int) calendar.Date { return calendar.Date{Year: y, Month: m, Day: d} }

// near reports whether got is within tolerance of want.
func near(t *testing.T, got money.Precise, want, tolerance string) bool {
	t.Helper()
	low, high := sum(t, dec(t, want), dec(t, "-"+tolerance)), sum(t, dec(t, want), dec(t, tolerance))
	return got.Cmp(low) >= 0 && got.Cmp(high) <= 0
}

func yearFraction(t *testing.T, dc products.DayCount, from, to calendar.Date) money.Precise {
	t.Helper()
	f, err := YearFraction(dc, from, to)
	if err != nil {
		t.Fatalf("YearFraction(%s, %v, %v): %v", dc, from, to, err)
	}
	return f
}

// TestACT365FCountsTheDays: 31 actual days are 31/365 of a year, also across a leap day, and a span of
// four centuries counts its 146,097 days.
func TestACT365FCountsTheDays(t *testing.T) {
	for _, span := range [][2]calendar.Date{{day(2026, 1, 1), day(2026, 2, 1)}, {day(2028, 2, 15), day(2028, 3, 17)}} {
		if got := yearFraction(t, products.ACT365F, span[0], span[1]); got.Cmp(ratio(t, "31", "365")) != 0 {
			t.Errorf("ACT/365F %v → %v = %s, want 31/365", span[0], span[1], got)
		}
	}
	if got := yearFraction(t, products.ACT365F, day(1700, 1, 1), day(2100, 1, 1)); got.Cmp(ratio(t, "146097", "365")) != 0 {
		t.Errorf("ACT/365F of four centuries = %s, want 146097/365", got)
	}
}

// TestACT360CountsTheDaysOver360: 31 actual days are 31/360.
func TestACT360CountsTheDaysOver360(t *testing.T) {
	if got := yearFraction(t, products.ACT360, day(2026, 1, 1), day(2026, 2, 1)); got.Cmp(ratio(t, "31", "360")) != 0 {
		t.Fatalf("ACT/360 of January = %s, want 31/360", got)
	}
}

// Test30E360GivesThirtyDaysAMonth: every whole month is 30/360, whatever its length (February included),
// and a 31st counts as the 30th at either end.
func Test30E360GivesThirtyDaysAMonth(t *testing.T) {
	months := [][2]calendar.Date{
		{day(2026, 1, 1), day(2026, 2, 1)},
		{day(2026, 2, 1), day(2026, 3, 1)},
		{day(2028, 2, 1), day(2028, 3, 1)},
		{day(2026, 1, 15), day(2026, 2, 15)},
		{day(2026, 3, 31), day(2026, 4, 30)},
		{day(2026, 12, 1), day(2027, 1, 1)},
	}
	for _, m := range months {
		if got := yearFraction(t, products.E30360, m[0], m[1]); got.Cmp(ratio(t, "30", "360")) != 0 {
			t.Errorf("30E/360 %v → %v = %s, want 30/360", m[0], m[1], got)
		}
	}
	// the 31st of January counts as the 30th, and the 28th of February stays: 28 days
	if got := yearFraction(t, products.E30360, day(2026, 1, 31), day(2026, 2, 28)); got.Cmp(ratio(t, "28", "360")) != 0 {
		t.Errorf("30E/360 from the 31st of January to the 28th of February = %s, want 28/360", got)
	}
	// an end on the 31st also counts as the 30th: two months
	if got := yearFraction(t, products.E30360, day(2026, 3, 30), day(2026, 5, 31)); got.Cmp(ratio(t, "60", "360")) != 0 {
		t.Errorf("30E/360 from the 30th of March to the 31st of May = %s, want 60/360", got)
	}
}

// TestACTACTSplitsByYear: ISDA counts each day over the length of its own year, so a span across a leap
// year adds days/365 and days/366, and a middle year counts whole.
func TestACTACTSplitsByYear(t *testing.T) {
	// 2027-12-01 → 2028-01-31: 31 days of 2027 (365) and 30 of 2028 (366)
	across := sum(t, ratio(t, "31", "365"), ratio(t, "30", "366"))
	if got := yearFraction(t, products.ACTACT, day(2027, 12, 1), day(2028, 1, 31)); got.Cmp(across) != 0 {
		t.Errorf("ACT/ACT across a leap year = %s, want %s", got, across)
	}
	if got := yearFraction(t, products.ACTACT, day(2028, 1, 1), day(2029, 1, 1)); got.Cmp(dec(t, "1")) != 0 {
		t.Errorf("ACT/ACT of a whole leap year = %s, want 1", got)
	}
	// 2027-07-01 → 2029-07-01: 184 days of 2027, all of 2028 and 181 days of 2029
	three := sum(t, sum(t, ratio(t, "184", "365"), ratio(t, "366", "366")), ratio(t, "181", "365"))
	if got := yearFraction(t, products.ACTACT, day(2027, 7, 1), day(2029, 7, 1)); got.Cmp(three) != 0 {
		t.Errorf("ACT/ACT over three calendar years = %s, want %s", got, three)
	}
}

// TestAYearFractionNeedsAnOrderedSpan: an unknown convention or a span that ends before it starts is an
// error; an empty span is 0.
func TestAYearFractionNeedsAnOrderedSpan(t *testing.T) {
	if _, err := YearFraction("30/360", day(2026, 1, 1), day(2026, 2, 1)); !errors.Is(err, ErrInvalidDayCount) {
		t.Errorf("unknown convention = %v, want ErrInvalidDayCount", err)
	}
	if _, err := YearFraction(products.ACT365F, day(2026, 2, 1), day(2026, 1, 1)); !errors.Is(err, ErrInvalidSpan) {
		t.Errorf("backwards span = %v, want ErrInvalidSpan", err)
	}
	if got := yearFraction(t, products.ACT360, day(2026, 2, 1), day(2026, 2, 1)); got.Cmp(money.Precise{}) != 0 {
		t.Errorf("empty span = %s, want 0", got)
	}
}

// TestEA2682IsNMV24 (SFC concepto 18759): 26.82 % EA is 24 % nominal monthly, within 0.01 point.
func TestEA2682IsNMV24(t *testing.T) {
	got, err := NominalFromEA(dec(t, "0.2682"), 12)
	if err != nil {
		t.Fatal(err)
	}
	if !near(t, got, "0.24", "0.0001") {
		t.Fatalf("NMV of 26.82 %% EA = %s, want 0.24 ± 0.0001", got)
	}
}

// TestAnEAIsNeverDividedBy12: 12 % EA is 0.9489 % a month and 2.8737 % a quarter, compounded, and not 1 %
// or 3 %.
func TestAnEAIsNeverDividedBy12(t *testing.T) {
	got, err := PeriodicRateFromEA(dec(t, "0.12"), 12)
	if err != nil {
		t.Fatal(err)
	}
	if !near(t, got, "0.009489", "0.000001") {
		t.Fatalf("monthly rate of 12 %% EA = %s, want 0.9489 %% (never 12 %% / 12 = 1 %%)", got)
	}
	quarterly, err := PeriodicRateFromEA(dec(t, "0.12"), 4)
	if err != nil {
		t.Fatal(err)
	}
	if !near(t, quarterly, "0.028737", "0.000001") {
		t.Fatalf("quarterly rate of 12 %% EA = %s, want 2.8737 %% (never 12 %% / 4 = 3 %%)", quarterly)
	}
}

// TestADailyRateCompoundsToTheEA: compounding the daily rate over the basis gives back the EA to 30
// decimals, for 365 and 360 days: precise enough for interest to the cent on any balance.
func TestADailyRateCompoundsToTheEA(t *testing.T) {
	for _, basis := range []int{365, 360} {
		daily, err := DailyRateFromEA(dec(t, "0.06"), basis)
		if err != nil {
			t.Fatal(err)
		}
		year, err := sum(t, daily, dec(t, "1")).Pow(dec(t, strconv.Itoa(basis)))
		if err != nil {
			t.Fatal(err)
		}
		if !near(t, year, "1.06", "0.000000000000000000000000000001") {
			t.Errorf("(1 + daily)^%d = %s, want 1.06", basis, year)
		}
	}
}

// TestTheRateOfADayFollowsItsConvention: under 30E/360 the 30th to the 31st weighs 0 and the 28th of
// February to the 1st of March weighs 3 days, so the rate of the day is the EA compounded over that
// fraction; a whole year gives the EA.
func TestTheRateOfADayFollowsItsConvention(t *testing.T) {
	ea := dec(t, "0.06")
	none, errNone := rateForFraction(ea, yearFraction(t, products.E30360, day(2026, 3, 30), day(2026, 3, 31)))
	three, errThree := rateForFraction(ea, yearFraction(t, products.E30360, day(2026, 2, 28), day(2026, 3, 1)))
	year, errYear := rateForFraction(ea, dec(t, "1"))
	daily, errDaily := DailyRateFromEA(ea, 360)
	if err := errors.Join(errNone, errThree, errYear, errDaily); err != nil {
		t.Fatal(err)
	}
	threeDays, err := sum(t, daily, dec(t, "1")).Pow(dec(t, "3"))
	if err != nil {
		t.Fatal(err)
	}
	if none.Cmp(money.Precise{}) != 0 || year.Cmp(ea) != 0 || !near(t, sum(t, three, dec(t, "1")), threeDays.String(), "0.000000000000000000000000000001") {
		t.Fatalf("rate of 0 days = %s, of 3 days = %s (want %s - 1), of a year = %s (want 0.06)", none, three, threeDays, year)
	}
}

// TestRatesNeedAValidBasisAndPeriod: a basis that is not 365 or 360, a period count that is not
// positive and an EA of -100 % or less are errors.
func TestRatesNeedAValidBasisAndPeriod(t *testing.T) {
	if _, err := DailyRateFromEA(dec(t, "0.06"), 366); !errors.Is(err, ErrInvalidBasis) {
		t.Errorf("basis 366 = %v, want ErrInvalidBasis", err)
	}
	for _, n := range []int{0, -12} {
		if _, err := PeriodicRateFromEA(dec(t, "0.06"), n); !errors.Is(err, ErrInvalidPeriods) {
			t.Errorf("%d periods = %v, want ErrInvalidPeriods", n, err)
		}
		if _, err := NominalFromEA(dec(t, "0.06"), n); !errors.Is(err, ErrInvalidPeriods) {
			t.Errorf("nominal over %d periods = %v, want ErrInvalidPeriods", n, err)
		}
	}
	if _, err := PeriodicRateFromEA(dec(t, "-1"), 12); !errors.Is(err, ErrInvalidRate) {
		t.Errorf("EA of -100 %% = %v, want ErrInvalidRate", err)
	}
}
