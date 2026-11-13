package domain

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/money"
)

var (
	// ErrNotProjected means a day without a projection of the GL: it is open, it had no transactions, or it comes
	// before the entity's first mapping.
	ErrNotProjected = errors.New("the day has no projection of the GL")
	// ErrDayUnmapped means a day projected without a GL: one of its accounts had no mapping in force, which the
	// control of the mappings reports.
	ErrDayUnmapped = errors.New("an account of the day has no CUIF mapping in force")
)

// DayStatus is how the projection of a day went.
type DayStatus string

// A day is projected with its lines, or unmapped and without them.
const (
	DayProjected DayStatus = "projected"
	DayUnmapped  DayStatus = "unmapped"
)

// AccountTotal is what an internal account moved on a day in a currency: its debits and its credits, both
// positive.
type AccountTotal struct {
	Account         string
	Debits, Credits money.Amount
}

// Line is a line of the daily GL: what the accounts of a CUIF code moved in a currency.
type Line struct {
	CUIFCode        string
	Debits, Credits money.Amount
}

// DailyGL is the projection of a day: how it went, when, and its lines.
type DailyGL struct {
	Status      DayStatus
	ProjectedAt time.Time
	Lines       []Line
}

type lineKey struct{ code, currency string }

// Project is the daily GL of day from what each account moved, by the CUIF code in force that day (ADR-0011),
// ordered by code and currency. One account without a mapping in force is ErrNoMapping: the day has no lines, since
// without it they would not sum zero.
func Project(totals []AccountTotal, mappings []Mapping, day calendar.Date) ([]Line, error) {
	byKey := make(map[lineKey]*Line)
	for _, t := range totals {
		code, err := Resolve(mappings, t.Account, day)
		if err != nil {
			return nil, err
		}
		key := lineKey{code, t.Debits.Currency().Code()}
		l, ok := byKey[key]
		if !ok {
			zero := money.New(0, t.Debits.Currency())
			l = &Line{CUIFCode: code, Debits: zero, Credits: zero}
			byKey[key] = l
		}
		if err := l.add(t); err != nil {
			return nil, fmt.Errorf("line %s %s: %w", key.code, key.currency, err)
		}
	}
	out := make([]Line, 0, len(byKey))
	for _, l := range byKey {
		out = append(out, *l)
	}
	slices.SortFunc(out, func(a, b Line) int {
		return cmp.Or(cmp.Compare(a.CUIFCode, b.CUIFCode), cmp.Compare(a.Debits.Currency().Code(), b.Debits.Currency().Code()))
	})
	return out, nil
}

func (l *Line) add(t AccountTotal) error {
	debits, err := l.Debits.Add(t.Debits)
	if err != nil {
		return err
	}
	credits, err := l.Credits.Add(t.Credits)
	if err != nil {
		return err
	}
	l.Debits, l.Credits = debits, credits
	return nil
}
