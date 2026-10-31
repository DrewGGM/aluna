// Package app holds the calendar's use cases.
package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/DrewGGM/aluna/internal/calendar/internal/domain"
	"github.com/DrewGGM/aluna/internal/ids"
)

var (
	// ErrLegalEntityNotFound means a legal entity that is not registered.
	ErrLegalEntityNotFound = errors.New("legal entity not found")
	// ErrStaleBusinessDate means a rollover whose expected date is no longer the business date: another
	// rollover advanced it.
	ErrStaleBusinessDate = errors.New("business date is no longer the expected one")
)

// Store persists the business date of each legal entity.
type Store interface {
	// BusinessDate reads the entity's business date, or ErrLegalEntityNotFound.
	BusinessDate(ctx context.Context, entity ids.ID) (domain.Date, error)
	// AdvanceBusinessDate changes it from expected to next only if it is still expected, and reports
	// whether it changed.
	AdvanceBusinessDate(ctx context.Context, entity ids.ID, expected, next domain.Date) (bool, error)
}

// Rollover advances the entity's business date from expected to the next business day (FR-13). It is a
// conditional UPDATE: it does not block postings that already read the date, and a second rollover with
// the same expected is ErrStaleBusinessDate instead of advancing two days.
func Rollover(ctx context.Context, store Store, entity ids.ID, expected domain.Date) (domain.Date, error) {
	next := domain.Adjust(dayAfter(expected), domain.Following, isColombianHoliday)
	advanced, err := store.AdvanceBusinessDate(ctx, entity, expected, next)
	if err != nil {
		return domain.Date{}, err
	}
	if advanced {
		return next, nil
	}
	current, err := store.BusinessDate(ctx, entity)
	if err != nil {
		return domain.Date{}, err
	}
	return domain.Date{}, fmt.Errorf("%w: is %s, expected %s", ErrStaleBusinessDate, iso(current), iso(expected))
}

func iso(d domain.Date) string { return fmt.Sprintf("%04d-%02d-%02d", d.Year, d.Month, d.Day) }

func dayAfter(d domain.Date) domain.Date {
	t := time.Date(d.Year, d.Month, d.Day+1, 0, 0, 0, 0, time.UTC)
	return domain.Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}
}

// isColombianHoliday is the entities' holiday calendar: all of them are Colombian for now.
func isColombianHoliday(d domain.Date) bool {
	return slices.Contains(domain.ColombianHolidays(d.Year), d)
}
