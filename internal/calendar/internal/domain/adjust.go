// Package domain es el dominio del calendario (ADR-0006): días calendario, festivos de Colombia y ajuste
// de fechas a día hábil.
package domain

import "time"

// Date es un día calendario, sin hora ni zona.
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

// Convention es cómo se mueve una fecha que cae en un día inhábil.
type Convention string

// Convenciones de ajuste de día hábil.
const (
	// Following lleva la fecha al siguiente día hábil.
	Following Convention = "following"
	// ModifiedFollowing la lleva al siguiente hábil, salvo que sea de otro mes: entonces al anterior.
	ModifiedFollowing Convention = "modified_following"
	// Preceding la lleva al día hábil anterior.
	Preceding Convention = "preceding"
	// Unadjusted no la mueve.
	Unadjusted Convention = "unadjusted"
)

// IsBusinessDay dice si d es hábil: de lunes a viernes y no festivo.
func IsBusinessDay(d Date, isHoliday func(Date) bool) bool {
	if wd := d.utc().Weekday(); wd == time.Saturday || wd == time.Sunday {
		return false
	}
	return !isHoliday(d)
}

// Adjust mueve d al día hábil que dice c; un d hábil no se mueve. No termina si isHoliday no deja ningún
// día hábil en la dirección de búsqueda. Una convención desconocida no mueve la fecha, como Unadjusted.
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

// nearest es el primer día hábil desde d (inclusive) avanzando de a step días.
func nearest(d Date, step int, isHoliday func(Date) bool) Date {
	for !IsBusinessDay(d, isHoliday) {
		d = d.addDays(step)
	}
	return d
}
