package app

import (
	"context"
	"errors"
	"fmt"

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
