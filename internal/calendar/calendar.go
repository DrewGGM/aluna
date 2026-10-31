// Package calendar is the calendar module (ADR-0006): legal entities, their business date and business
// days.
package calendar

import (
	"context"

	pg "github.com/DrewGGM/aluna/internal/calendar/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/calendar/internal/app"
	"github.com/DrewGGM/aluna/internal/calendar/internal/domain"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// Date is a calendar day, with no time or zone.
type Date = domain.Date

// PeriodStatus is the status of an accounting period.
type PeriodStatus = app.PeriodStatus

// Statuses of an accounting period.
const (
	PeriodOpen    = app.PeriodOpen
	PeriodClosing = app.PeriodClosing
	PeriodClosed  = app.PeriodClosed
)

// Calendar errors.
var (
	ErrLegalEntityNotFound = app.ErrLegalEntityNotFound
	ErrStaleBusinessDate   = app.ErrStaleBusinessDate
	ErrPeriodClosed        = app.ErrPeriodClosed
)

// Service is the calendar facade. It holds no state: each operation receives the connection or the
// transaction it runs in.
type Service struct{}

// New creates the calendar service.
func New() *Service { return &Service{} }

// BusinessDate is the business date of the legal entity. A use case reads it when its transaction starts
// and uses it for everything it posts: the read does not block the rollover. q can be the pool or a Tx.
func (s *Service) BusinessDate(ctx context.Context, q postgres.Querier, legalEntity ids.ID) (Date, error) {
	return pg.NewStore(q).BusinessDate(ctx, legalEntity)
}

// Rollover advances, within tx, the entity's business date from expected to the next business day and
// returns the new one. If the date is no longer expected (another rollover advanced it), it is
// ErrStaleBusinessDate and nothing changes.
func (s *Service) Rollover(ctx context.Context, tx postgres.Tx, legalEntity ids.ID, expected Date) (Date, error) {
	return app.Rollover(ctx, pg.NewStore(tx), legalEntity, expected)
}

// PeriodStatus is the status of the entity's accounting period for d (FR-15). A month the close has not
// touched is open. q can be the pool or a Tx.
func (s *Service) PeriodStatus(ctx context.Context, q postgres.Querier, legalEntity ids.ID, d Date) (PeriodStatus, error) {
	return app.PeriodStatusOf(ctx, pg.NewStore(q), legalEntity, d)
}

// EnsureOpen is ErrPeriodClosed if d falls in a closed period of the entity: the ledger asks for it
// before posting. It reads without locking, like BusinessDate.
func (s *Service) EnsureOpen(ctx context.Context, q postgres.Querier, legalEntity ids.ID, d Date) error {
	return app.EnsureOpen(ctx, pg.NewStore(q), legalEntity, d)
}
