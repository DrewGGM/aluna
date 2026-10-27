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
	// Festivos que se celebran en su fecha.
	fixedHolidays = []monthDay{
		{time.January, 1}, {time.May, 1}, {time.July, 20}, {time.August, 7}, {time.December, 8}, {time.December, 25},
	}
	// Festivos que la Ley 51 de 1983 (Emiliani) traslada al lunes siguiente si no caen en lunes.
	mondayHolidays = []monthDay{
		{time.January, 6},
		{time.March, 19},
		{time.June, 29},
		{time.August, 15},
		{time.October, 12},
		{time.November, 1},
		{time.November, 11},
	}
	// Días desde la Pascua: Jueves y Viernes Santo quedan en su fecha; Ascensión, Corpus Christi y
	// Sagrado Corazón se trasladan al lunes.
	easterHolidays       = []int{-3, -2}
	easterMondayHolidays = []int{39, 60, 68}
	// Nuestra Señora del Rosario de Chiquinquirá (Ley 2578 de 2026), trasladable al lunes, desde 2026.
	chiquinquira      = monthDay{time.July, 9}
	chiquinquiraSince = 2026
)

// ColombianHolidays son los festivos nacionales de Colombia en year (Ley 51 de 1983 y Ley 2578 de 2026),
// en orden. Dos
// festivos que caen el mismo lunes (el Sagrado Corazón y San Pedro y San Pablo algunos años) son una
// sola fecha. Las reglas valen desde 1984, cuando rige la Ley 51.
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

// nextMonday es d si es lunes; si no, el lunes siguiente.
func nextMonday(d Date) Date {
	return d.addDays((int(time.Monday) - int(d.utc().Weekday()) + 7) % 7)
}

// easter es el domingo de Pascua del calendario gregoriano (algoritmo anónimo de Meeus/Jones/Butcher).
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
