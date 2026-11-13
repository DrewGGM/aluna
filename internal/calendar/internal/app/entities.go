package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/DrewGGM/aluna/internal/calendar/internal/domain"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
)

// ErrInvalidLegalEntity means a legal entity without a name, with an unknown functional currency or whose
// business date is not a business day or has not started yet.
var ErrInvalidLegalEntity = errors.New("invalid legal entity")

// LegalEntity is a legal entity to register: its name, its first business date and its functional currency.
type LegalEntity struct {
	ID           ids.ID
	Name         string
	BusinessDate domain.Date
	Currency     string
}

// EntityStore registers legal entities.
type EntityStore interface {
	CreateLegalEntity(ctx context.Context, e LegalEntity) error
}

// CreateLegalEntity registers e, with its name trimmed. Its business date must be a business day, since the rollover
// only ever moves to one and postings dated on a weekend or a holiday would land in a day no rollover closes; and it
// must have started in America/Bogota at now, like the day a rollover closes: the business date never runs ahead of
// the calendar.
func CreateLegalEntity(ctx context.Context, store EntityStore, e LegalEntity, now time.Time) error {
	if e.Name = strings.TrimSpace(e.Name); e.Name == "" {
		return fmt.Errorf("%w: empty name", ErrInvalidLegalEntity)
	}
	if !domain.IsBusinessDay(e.BusinessDate, isColombianHoliday) {
		return fmt.Errorf("%w: %s is not a business day", ErrInvalidLegalEntity, e.BusinessDate)
	}
	if today := domain.Today(now); e.BusinessDate.After(today) {
		return fmt.Errorf("%w: %w: %s, today is %s", ErrInvalidLegalEntity, ErrBusinessDateAhead, e.BusinessDate, today)
	}
	if _, err := money.CurrencyOf(e.Currency); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidLegalEntity, err)
	}
	return store.CreateLegalEntity(ctx, e)
}
