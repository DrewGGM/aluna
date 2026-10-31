package app

import (
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/calendar"
)

func day(year int, month time.Month, d int) calendar.Date {
	return calendar.Date{Year: year, Month: month, Day: d}
}

// TestTheCapitalizationDateIsThePeriodsLastDayAdjusted (FR-26): a period is capitalized on its last day,
// moved to a business day as the product's convention says (Colombian holidays); the date moved forward
// lands in the next period and still capitalizes the one it closes. Any other day capitalizes nothing.
func TestTheCapitalizationDateIsThePeriodsLastDayAdjusted(t *testing.T) {
	adjust := calendar.New().AdjustDate
	for name, c := range map[string]struct {
		day        calendar.Date
		frequency  string
		convention calendar.Convention
		period     string // empty: not a capitalization date
	}{
		"end of a month on a business day":    {day(2026, time.April, 30), "monthly", calendar.Following, "2026-04"},
		"a day before it":                     {day(2026, time.April, 29), "monthly", calendar.Following, ""},
		"a Sunday month end, unadjusted":      {day(2026, time.May, 31), "monthly", calendar.Unadjusted, "2026-05"},
		"a Sunday month end, moved to Monday": {day(2026, time.June, 1), "monthly", calendar.Following, "2026-05"},
		"the Sunday itself, when moved":       {day(2026, time.May, 31), "monthly", calendar.Following, ""},
		"a Sunday month end, moved back":      {day(2026, time.May, 29), "monthly", calendar.Preceding, "2026-05"},
		"modified following stays in month":   {day(2026, time.May, 29), "monthly", calendar.ModifiedFollowing, "2026-05"},
		"end of a quarter":                    {day(2026, time.June, 30), "quarterly", calendar.Following, "2026-Q2"},
		"a month end that is not a quarter's": {day(2026, time.April, 30), "quarterly", calendar.Following, ""},
		"end of a half":                       {day(2026, time.December, 31), "semiannual", calendar.Unadjusted, "2026-H2"},
		"end of the first half":               {day(2026, time.June, 30), "semiannual", calendar.Following, "2026-H1"},
		"end of a year":                       {day(2026, time.December, 31), "annual", calendar.Unadjusted, "2026"},
		"a year end moved into the next year": {day(2029, time.January, 2), "annual", calendar.Following, "2028"},
	} {
		period, ok, err := capitalizationOn(c.day, c.frequency, c.convention, adjust)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if ok != (c.period != "") || period != c.period {
			t.Errorf("%s: %v capitalizes %q (%v), want %q", name, c.day, period, ok, c.period)
		}
	}
}

// TestAnUnknownFrequencyIsAnError: a frequency the definition does not allow is not taken for a period.
func TestAnUnknownFrequencyIsAnError(t *testing.T) {
	if _, _, err := capitalizationOn(day(2026, time.April, 30), "weekly", calendar.Following, calendar.New().AdjustDate); err == nil {
		t.Fatal("weekly capitalization accepted, want an error")
	}
}
