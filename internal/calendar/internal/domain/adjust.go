// Package domain is the calendar domain (ADR-0006): calendar days, Colombian holidays and adjustment of
// dates to business days.
package domain

import (
	"errors"
	"fmt"
	"time"
)

// ErrInvalidDate means a text that is not a date yyyy-mm-dd.
var ErrInvalidDate = errors.New("invalid date")

// Date is a calendar day, with no time or zone.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

// String is d as yyyy-mm-dd, the API's and the events' format.
func (d Date) String() string { return fmt.Sprintf("%04d-%02d-%02d", d.Year, d.Month, d.Day) }

// ParseDate reads a date as String writes it, yyyy-mm-dd, or ErrInvalidDate.
func ParseDate(s string) (Date, error) {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return Date{}, fmt.Errorf("%w: %q is not yyyy-mm-dd", ErrInvalidDate, s)
	}
	return DateOf(t), nil
}

// DateOf is the day of t in its own location.
func DateOf(t time.Time) Date { return Date{Year: t.Year(), Month: t.Month(), Day: t.Day()} }

// After reports whether d is a later day than o.
func (d Date) After(o Date) bool { return d.Time().After(o.Time()) }

// Time is d at midnight UTC: how pgx writes a date column, and the ledger's value and business dates.
func (d Date) Time() time.Time { return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC) }

// AddDays is the day n days after d (before it, if n is negative).
func (d Date) AddDays(n int) Date { return DateOf(d.Time().AddDate(0, 0, n)) }

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
	if wd := d.Time().Weekday(); wd == time.Saturday || wd == time.Sunday {
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
		d = d.AddDays(step)
	}
	return d
}
