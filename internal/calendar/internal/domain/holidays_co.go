package domain

import (
	"slices"
	"time"
)

type monthDay struct {
	month time.Month
	day   int
}

var (
	// Holidays observed on their date.
	fixedHolidays = []monthDay{
		{time.January, 1}, {time.May, 1}, {time.July, 20}, {time.August, 7}, {time.December, 8}, {time.December, 25},
	}
	// Holidays that Law 51 of 1983 (Emiliani) moves to the following Monday if they do not fall on one.
	mondayHolidays = []monthDay{
		{time.January, 6},
		{time.March, 19},
		{time.June, 29},
		{time.August, 15},
		{time.October, 12},
		{time.November, 1},
		{time.November, 11},
	}
	// Days from Easter: Maundy Thursday and Good Friday stay on their date; Ascension, Corpus Christi and
	// Sacred Heart move to the Monday.
	easterHolidays       = []int{-3, -2}
	easterMondayHolidays = []int{39, 60, 68}
	// Our Lady of the Rosary of Chiquinquirá (Law 2578 of 2026), moved to the Monday, since 2026.
	chiquinquira      = monthDay{time.July, 9}
	chiquinquiraSince = 2026
)

// ColombianHolidays are Colombia's national holidays in year (Law 51 of 1983 and Law 2578 of 2026), in
// order. Two holidays that fall on the same Monday (Sacred Heart and Saints Peter and Paul in some years)
// are a single date. The rules hold since 1984, when Law 51 took effect.
func ColombianHolidays(year int) []Date {
	var days []Date
	for _, md := range fixedHolidays {
		days = append(days, Date{year, md.month, md.day})
	}
	for _, md := range mondayHolidays {
		days = append(days, nextMonday(Date{year, md.month, md.day}))
	}
	if year >= chiquinquiraSince {
		days = append(days, nextMonday(Date{year, chiquinquira.month, chiquinquira.day}))
	}
	e := easter(year)
	for _, offset := range easterHolidays {
		days = append(days, e.addDays(offset))
	}
	for _, offset := range easterMondayHolidays {
		days = append(days, nextMonday(e.addDays(offset)))
	}
	slices.SortFunc(days, func(a, b Date) int { return a.utc().Compare(b.utc()) })
	return slices.Compact(days)
}

// nextMonday is d if it is a Monday; otherwise, the following Monday.
func nextMonday(d Date) Date {
	return d.addDays((int(time.Monday) - int(d.utc().Weekday()) + 7) % 7)
}

// easter is Easter Sunday in the Gregorian calendar (the anonymous Meeus/Jones/Butcher algorithm).
func easter(year int) Date {
	a, b, c := year%19, year/100, year%100
	d, e := b/4, b%4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	h := (19*a + b - d - g + 15) % 30
	i, k := c/4, c%4
	l := (32 + 2*e + 2*i - h - k) % 7
	m := (a + 11*h + 22*l) / 451
	n := h + l - 7*m + 114
	return Date{year, time.Month(n / 31), n%31 + 1}
}
