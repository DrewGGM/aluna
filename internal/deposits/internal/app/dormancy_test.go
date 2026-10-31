package app

import (
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/calendar"
)

// TestDormantBefore (FR-27): an account is dormant once its last customer activity is before the business
// date minus the product's days, across months, years and a leap day; exactly that many days is not dormant.
func TestDormantBefore(t *testing.T) {
	for name, c := range map[string]struct {
		today calendar.Date
		days  int
		want  calendar.Date
	}{
		"30 days into the month before":  {day(2026, time.April, 2), 30, day(2026, time.March, 3)},
		"a year back":                    {day(2026, time.March, 2), 365, day(2025, time.March, 2)},
		"across a leap day":              {day(2028, time.March, 1), 1, day(2028, time.February, 29)},
		"the longest the products allow": {day(2026, time.March, 2), 3650, day(2016, time.March, 4)},
	} {
		if got := dormantBefore(c.today, c.days); got != c.want {
			t.Errorf("%s: dormant before %v, want %v", name, got, c.want)
		}
	}
}
