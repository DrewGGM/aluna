// Package domain is the calendar domain (ADR-0006): calendar days, Colombian holidays and adjustment of
// dates to business days.
package domain

import "time"

// Date is a calendar day, with no time or zone.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

func (d Date) utc() time.Time { return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC) }

func (d Date) addDays(n int) Date {
	t := d.utc().AddDate(0, 0, n)
	return Date{t.Year(), t.Month(), t.Day()}
}

// Convention is how a date that falls on a non-business day is moved.
type Convention string

// Business day adjustment conventions.
const (
	// Following moves the date to the next business day.
	Following Convention = "following"
	// ModifiedFollowing moves it to the next business day, unless that is in another month: then to the
	// previous one.
	ModifiedFollowing Convention = "modified_following"
	// Preceding moves it to the previous business day.
	Preceding Convention = "preceding"
	// Unadjusted does not move it.
	Unadjusted Convention = "unadjusted"
)

// IsBusinessDay reports whether d is a business day: Monday to Friday and not a holiday.
func IsBusinessDay(d Date, isHoliday func(Date) bool) bool {
	if wd := d.utc().Weekday(); wd == time.Saturday || wd == time.Sunday {
		return false
	}
	return !isHoliday(d)
}

// Adjust moves d to the business day that c dictates; a d that is a business day is not moved. It does
// not terminate if isHoliday leaves no business day in the search direction. An unknown convention does
// not move the date, like Unadjusted.
func Adjust(d Date, c Convention, isHoliday func(Date) bool) Date {
	switch c {
	case Following:
		return nearest(d, 1, isHoliday)
	case Preceding:
		return nearest(d, -1, isHoliday)
	case ModifiedFollowing:
		if next := nearest(d, 1, isHoliday); next.Month == d.Month {
			return next
		}
		return nearest(d, -1, isHoliday)
	case Unadjusted:
	}
	return d
}

// nearest is the first business day from d (inclusive), moving step days at a time.
func nearest(d Date, step int, isHoliday func(Date) bool) Date {
	for !IsBusinessDay(d, isHoliday) {
		d = d.addDays(step)
	}
	return d
}
