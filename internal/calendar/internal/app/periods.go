package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/DrewGGM/aluna/internal/calendar/internal/domain"
	"github.com/DrewGGM/aluna/internal/ids"
)

// PeriodStatus is the status of an accounting period (FR-15).
type PeriodStatus string

// Statuses of an accounting period: open → closing → closed.
const (
	PeriodOpen    PeriodStatus = "open"
	PeriodClosing PeriodStatus = "closing"
	PeriodClosed  PeriodStatus = "closed"
)

// ErrPeriodClosed means a business date inside a closed period: nothing is posted.
var ErrPeriodClosed = errors.New("accounting period closed")

// PeriodStore reads the status of periods.
type PeriodStore interface {
	// PeriodStatus is the status of the entity's yyyy-mm period; found is false if it has no row.
	PeriodStatus(ctx context.Context, entity ids.ID, period string) (status PeriodStatus, found bool, err error)
}

// PeriodStatusOf is the status of d's period. A month with no row is open: the close creates the row.
func PeriodStatusOf(ctx context.Context, store PeriodStore, entity ids.ID, d domain.Date) (PeriodStatus, error) {
	status, found, err := store.PeriodStatus(ctx, entity, periodOf(d))
	if err != nil {
		return "", err
	}
	if !found {
		return PeriodOpen, nil
	}
	return status, nil
}

// EnsureOpen is ErrPeriodClosed if d falls in a closed period. A period that is closing still accepts
// postings (the closing adjustments).
func EnsureOpen(ctx context.Context, store PeriodStore, entity ids.ID, d domain.Date) error {
	status, err := PeriodStatusOf(ctx, store, entity, d)
	if err != nil {
		return err
	}
	if status == PeriodClosed {
		return fmt.Errorf("%w: %s", ErrPeriodClosed, periodOf(d))
	}
	return nil
}

func periodOf(d domain.Date) string { return fmt.Sprintf("%04d-%02d", d.Year, d.Month) }

var (
	// ErrInvalidPeriod means a period that is not a month, yyyy-mm.
	ErrInvalidPeriod = errors.New("a period is a month, yyyy-mm")
	// ErrPeriodNotEnded means a period that is not over: the entity's business date is in it or before it.
	ErrPeriodNotEnded = errors.New("the period has not ended")
)

// PeriodCloser is what closing a period needs.
type PeriodCloser interface {
	PeriodStore
	BusinessDate(ctx context.Context, entity ids.ID) (domain.Date, error)
	// MarkClosing moves the entity's period from open (or no row) to closing; false if it was not open.
	MarkClosing(ctx context.Context, entity ids.ID, period string) (bool, error)
	// MarkClosed moves the entity's closing period to closed, by who.
	MarkClosed(ctx context.Context, entity ids.ID, period, by string) error
}

// CheckClose reports whether the entity's period can close (FR-37): ErrInvalidPeriod if it is not a month,
// ErrPeriodNotEnded if the business date has not left it, ErrPeriodClosed if it is closed or closing.
func CheckClose(ctx context.Context, s PeriodCloser, entity ids.ID, period string) error {
	_, _, err := bounds(ctx, s, entity, period)
	if err != nil {
		return err
	}
	status, found, err := s.PeriodStatus(ctx, entity, period)
	if err != nil {
		return err
	}
	if found && status != PeriodOpen {
		return fmt.Errorf("%w: %s is %s", ErrPeriodClosed, period, status)
	}
	return nil
}

// ClosePeriod closes the entity's period, by who (FR-37): it must be able to close (CheckClose), it goes closing, verify
// checks it from its first day to the first of the next month, excluded, and it is closed. Another close that took it
// meanwhile is ErrPeriodClosed. If verify fails, the period is left closing in the caller's transaction, which undoes
// it.
func ClosePeriod(ctx context.Context, s PeriodCloser, entity ids.ID, period, by string, verify func(ctx context.Context, from, to domain.Date) error) error {
	from, to, err := bounds(ctx, s, entity, period)
	if err != nil {
		return err
	}
	marked, err := s.MarkClosing(ctx, entity, period)
	if err != nil {
		return err
	}
	if !marked {
		return fmt.Errorf("%w: %s", ErrPeriodClosed, period)
	}
	if err := verify(ctx, from, to); err != nil {
		return err
	}
	return s.MarkClosed(ctx, entity, period, by)
}

// ParsePeriod is the first instant of period, a month as yyyy-mm, or ErrInvalidPeriod.
func ParsePeriod(period string) (time.Time, error) {
	start, err := time.Parse("2006-01", period)
	if err != nil {
		return time.Time{}, ErrInvalidPeriod
	}
	return start, nil
}

// bounds are the first day of period and the first of the next month, if it is a month that has ended: the
// entity's business date is after it.
func bounds(ctx context.Context, s PeriodCloser, entity ids.ID, period string) (from, to domain.Date, err error) {
	start, err := ParsePeriod(period)
	if err != nil {
		return domain.Date{}, domain.Date{}, err
	}
	next := start.AddDate(0, 1, 0)
	today, err := s.BusinessDate(ctx, entity)
	if err != nil {
		return domain.Date{}, domain.Date{}, err
	}
	if time.Date(today.Year, today.Month, today.Day, 0, 0, 0, 0, time.UTC).Before(next) {
		return domain.Date{}, domain.Date{}, fmt.Errorf("%w: %s, and the business date is %04d-%02d-%02d", ErrPeriodNotEnded, period,
			today.Year, today.Month, today.Day)
	}
	return domain.Date{Year: start.Year(), Month: start.Month(), Day: 1}, domain.Date{Year: next.Year(), Month: next.Month(), Day: 1}, nil
}
