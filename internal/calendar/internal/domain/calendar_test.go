package domain

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"
)

// officialHolidays are each year's national holidays (Law 51 of 1983 and, since 2026, Law 2578 of 2026:
// Our Lady of the Rosary of Chiquinquirá), as month-day. There are 18 holidays up to 2025 and 19 since
// 2026; in 2025, 2030 and 2038 Sacred Heart and Saints Peter and Paul fall on the same Monday, so there
// is one date fewer.
var officialHolidays = map[int]string{
	2025: "01-01 01-06 03-24 04-17 04-18 05-01 06-02 06-23 06-30 07-20 08-07 08-18 10-13 11-03 11-17 12-08 12-25",
	2026: "01-01 01-12 03-23 04-02 04-03 05-01 05-18 06-08 06-15 06-29 07-13 07-20 08-07 08-17 10-12 11-02 11-16 12-08 12-25",
	2027: "01-01 01-11 03-22 03-25 03-26 05-01 05-10 05-31 06-07 07-05 07-12 07-20 08-07 08-16 10-18 11-01 11-15 12-08 12-25",
	2028: "01-01 01-10 03-20 04-13 04-14 05-01 05-29 06-19 06-26 07-03 07-10 07-20 08-07 08-21 10-16 11-06 11-13 12-08 12-25",
	2029: "01-01 01-08 03-19 03-29 03-30 05-01 05-14 06-04 06-11 07-02 07-09 07-20 08-07 08-20 10-15 11-05 11-12 12-08 12-25",
	2030: "01-01 01-07 03-25 04-18 04-19 05-01 06-03 06-24 07-01 07-15 07-20 08-07 08-19 10-14 11-04 11-11 12-08 12-25",
	2038: "01-01 01-11 03-22 04-22 04-23 05-01 06-07 06-28 07-05 07-12 07-20 08-07 08-16 10-18 11-01 11-15 12-08 12-25",
}

func TestColombianHolidaysMatchTheOfficialCalendar(t *testing.T) {
	for year, days := range officialHolidays {
		t.Run(strconv.Itoa(year), func(t *testing.T) {
			var want []Date
			for md := range strings.FieldsSeq(days) {
				var m, d int
				if _, err := fmt.Sscanf(md, "%d-%d", &m, &d); err != nil {
					t.Fatal(err)
				}
				want = append(want, Date{Year: year, Month: time.Month(m), Day: d})
			}
			if got := ColombianHolidays(year); !slices.Equal(got, want) {
				t.Errorf("\n got %v\nwant %v", got, want)
			}
		})
	}
}

func TestEaster(t *testing.T) {
	for _, want := range []Date{{2008, time.March, 23}, {2011, time.April, 24}, {2025, time.April, 20}, {2026, time.April, 5}, {2038, time.April, 25}} {
		if got := easter(want.Year); got != want {
			t.Errorf("easter %d = %v, want %v", want.Year, got, want)
		}
	}
}

// TestEasterIsASundayInItsWindow: across the whole Gregorian calendar, Easter is a Sunday between March
// 22 and April 25.
func TestEasterIsASundayInItsWindow(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		e := easter(rapid.IntRange(1583, 4099).Draw(t, "year"))
		earliest, latest := Date{e.Year, time.March, 22}, Date{e.Year, time.April, 25}
		if e.utc().Weekday() != time.Sunday || e.utc().Before(earliest.utc()) || e.utc().After(latest.utc()) {
			t.Fatalf("easter %v", e)
		}
	})
}

func isColombianHoliday(d Date) bool { return slices.Contains(ColombianHolidays(d.Year), d) }

func TestAdjust(t *testing.T) {
	cases := []struct {
		name string
		from Date
		c    Convention
		want Date
	}{
		// Saturday June 28, 2025 comes before Monday the 30th, a holiday
		{"following skips the weekend and the holiday", Date{2025, time.June, 28}, Following, Date{2025, time.July, 1}},
		{"preceding goes back to friday", Date{2025, time.June, 28}, Preceding, Date{2025, time.June, 27}},
		{"unadjusted does not move", Date{2025, time.June, 28}, Unadjusted, Date{2025, time.June, 28}},
		{"an unknown convention does not move", Date{2025, time.June, 28}, Convention("x"), Date{2025, time.June, 28}},
		{"a business day does not move", Date{2025, time.June, 27}, Following, Date{2025, time.June, 27}},
		{"modified following goes back if the next business day is in another month", Date{2025, time.June, 28}, ModifiedFollowing, Date{2025, time.June, 27}},
		// Saturday May 31, 2025: the next business day is June 3 (the 2nd is Ascension)
		{"modified following does not change month", Date{2025, time.May, 31}, ModifiedFollowing, Date{2025, time.May, 30}},
		// Saturday June 21, 2025: Monday the 23rd is Corpus Christi
		{"modified following within the month", Date{2025, time.June, 21}, ModifiedFollowing, Date{2025, time.June, 24}},
		{"modified following from the 1st to the next business day", Date{2025, time.June, 1}, ModifiedFollowing, Date{2025, time.June, 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Adjust(tc.from, tc.c, isColombianHoliday); got != tc.want {
				t.Errorf("Adjust(%v, %s) = %v, want %v", tc.from, tc.c, got, tc.want)
			}
		})
	}
}

func TestIsBusinessDay(t *testing.T) {
	cases := map[string]struct {
		d    Date
		want bool
	}{
		"friday":   {Date{2025, time.June, 27}, true},
		"saturday": {Date{2025, time.June, 28}, false},
		"sunday":   {Date{2025, time.June, 29}, false},
		"holiday":  {Date{2025, time.June, 30}, false},
		"tuesday":  {Date{2025, time.July, 1}, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := IsBusinessDay(tc.d, isColombianHoliday); got != tc.want {
				t.Errorf("IsBusinessDay(%v) = %v, want %v", tc.d, got, tc.want)
			}
		})
	}
}

// holidaysGen is Colombia's holidays plus up to 10 random non-business days around from.
func holidaysGen(t *rapid.T, from Date) func(Date) bool {
	extra := rapid.SliceOfN(rapid.IntRange(-20, 40), 0, 10).Draw(t, "nonBusinessDays")
	return func(d Date) bool {
		for _, off := range extra {
			if from.addDays(off) == d {
				return true
			}
		}
		return isColombianHoliday(d)
	}
}

func dateGen(t *rapid.T) Date {
	day := rapid.IntRange(0, 365*120).Draw(t, "day")
	return Date{1990, time.January, 1}.addDays(day)
}

// TestAdjustProperties: Following and Preceding return the first business day forward or backward;
// ModifiedFollowing returns Following's if it is in the same month and Preceding's otherwise, and only
// changes month if the whole rest of the month has no business day.
func TestAdjustProperties(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		from := dateGen(t)
		isHoliday := holidaysGen(t, from)
		following, preceding := Adjust(from, Following, isHoliday), Adjust(from, Preceding, isHoliday)
		checkNearest(t, from, following, 1, isHoliday)
		checkNearest(t, from, preceding, -1, isHoliday)
		if got := Adjust(from, Unadjusted, isHoliday); got != from {
			t.Fatalf("Unadjusted(%v) = %v", from, got)
		}
		want := preceding
		if following.Month == from.Month {
			want = following
		}
		got := Adjust(from, ModifiedFollowing, isHoliday)
		if got != want {
			t.Fatalf("ModifiedFollowing(%v) = %v, want %v", from, got, want)
		}
		if got.Month != from.Month {
			for d := from; d.Month == from.Month; d = d.addDays(1) {
				if IsBusinessDay(d, isHoliday) {
					t.Fatalf("ModifiedFollowing(%v) = %v changed month, but %v is a business day", from, got, d)
				}
			}
		}
	})
}

// checkNearest: got is a business day and the first one from from in direction step (1 forward, -1
// backward).
func checkNearest(t *rapid.T, from, got Date, step int, isHoliday func(Date) bool) {
	if !IsBusinessDay(got, isHoliday) {
		t.Fatalf("Adjust(%v, step %d) = %v, not a business day", from, step, got)
	}
	for d := from; d != got; d = d.addDays(step) {
		if IsBusinessDay(d, isHoliday) {
			t.Fatalf("Adjust(%v, step %d) = %v, but %v is an earlier business day", from, step, got, d)
		}
	}
}
