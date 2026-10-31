package interest

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/products"
)

var (
	// ErrInvalidDayCount means a day count convention this package does not know.
	ErrInvalidDayCount = errors.New("unknown day count convention")
	// ErrInvalidSpan means a span that ends before it starts.
	ErrInvalidSpan = errors.New("the span ends before it starts")
)

// fractionScale is the number of decimals of a year fraction: far below a minor unit of any balance.
const fractionScale = 30

// YearFraction is the fraction of a year from from (included) to to (excluded) under day count dc:
// ACT/365F and ACT/360 count the actual days over 365 or 360; 30E/360 counts 30 days a month, with a 31st
// as the 30th; ACT/ACT (ISDA) counts each day over the length of its own year.
func YearFraction(dc products.DayCount, from, to calendar.Date) (money.Precise, error) {
	first, last := ordinal(from), ordinal(to)
	if last < first {
		return money.Precise{}, fmt.Errorf("%w: %v to %v", ErrInvalidSpan, from, to)
	}
	switch dc {
	case products.ACT365F:
		return fraction(last-first, 365)
	case products.ACT360:
		return fraction(last-first, 360)
	case products.E30360:
		return fraction(days30E(from, to), 360)
	case products.ACTACT:
		return actualOverActual(from.Year, to.Year, first, last)
	}
	return money.Precise{}, fmt.Errorf("%w: %q", ErrInvalidDayCount, dc)
}

// actualOverActual adds, for each calendar year from fromYear to toYear, the days of [first, last) in it
// over the year's length (365 or 366).
func actualOverActual(fromYear, toYear, first, last int) (money.Precise, error) {
	var total money.Precise
	for year := fromYear; year <= toYear; year++ {
		start, next := newYear(year), newYear(year+1)
		part, err := fraction(min(last, next)-max(first, start), next-start)
		if err != nil {
			return money.Precise{}, err
		}
		if total, err = total.Add(part); err != nil {
			return money.Precise{}, err
		}
	}
	return total, nil
}

// days30E counts 30 days a month and 360 a year, with a 31st as the 30th (30E/360, Eurobond basis).
func days30E(from, to calendar.Date) int {
	return 360*(to.Year-from.Year) + 30*int(to.Month-from.Month) + min(to.Day, 30) - min(from.Day, 30)
}

// ordinal is d as days since 1970-01-01, so spans and clamps are integer arithmetic.
func ordinal(d calendar.Date) int {
	return int(time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC).Unix() / secondsPerDay)
}

const secondsPerDay = 24 * 60 * 60

func newYear(year int) int { return ordinal(calendar.Date{Year: year, Month: time.January, Day: 1}) }

// fraction is days/basis with fractionScale decimals.
func fraction(days, basis int) (money.Precise, error) { return quotient(days, basis, fractionScale) }

// quotient is a/b with scale decimals, rounded half even.
func quotient(a, b, scale int) (money.Precise, error) {
	pa, err := integer(a)
	if err != nil {
		return money.Precise{}, err
	}
	pb, err := integer(b)
	if err != nil {
		return money.Precise{}, err
	}
	return pa.Quo(pb, scale, money.HalfEven)
}

func integer(n int) (money.Precise, error) { return precise64(int64(n)) }

func precise64(n int64) (money.Precise, error) { return money.ParsePrecise(strconv.FormatInt(n, 10)) }
