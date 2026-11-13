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

// Convention is how a date that falls on a non-business day is moved.
type Convention = domain.Convention

// Business day adjustment conventions.
const (
	Following         = domain.Following
	ModifiedFollowing = domain.ModifiedFollowing
	Preceding         = domain.Preceding
	Unadjusted        = domain.Unadjusted
)

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
	ErrInvalidPeriod       = app.ErrInvalidPeriod
	ErrPeriodNotEnded      = app.ErrPeriodNotEnded
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

// AdjustDate moves d to the business day that convention c dictates (a business day stays): a product's
// capitalization date, for example. The holidays are Colombia's, the entities' calendar.
func (s *Service) AdjustDate(d Date, c Convention) Date { return app.AdjustDate(d, c) }

// PeriodStatus is the status of the entity's accounting period for d (FR-15). A month the close has not
// touched is open. q can be the pool or a Tx.
func (s *Service) PeriodStatus(ctx context.Context, q postgres.Querier, legalEntity ids.ID, d Date) (PeriodStatus, error) {
	return app.PeriodStatusOf(ctx, pg.NewStore(q), legalEntity, d)
}

// ValidatePeriod is ErrInvalidPeriod if period is not a month, yyyy-mm.
func ValidatePeriod(period string) error {
	_, err := app.ParsePeriod(period)
	return err
}

// CheckPeriodClose reports whether the entity's period, yyyy-mm, can close (FR-37): ErrInvalidPeriod if it is not
// a month, ErrPeriodNotEnded if the business date has not left it, ErrPeriodClosed if it is closed or closing. q can
// be the pool or a Tx.
func (s *Service) CheckPeriodClose(ctx context.Context, q postgres.Querier, legalEntity ids.ID, period string) error {
	return app.CheckClose(ctx, pg.NewStore(q), legalEntity, period)
}

// ClosePeriod closes the entity's period within tx, by who (FR-37): with the errors of CheckPeriodClose, or
// ErrPeriodClosed if another close took it, it goes closing; verify checks it from its first day to the first of the
// next month, excluded, while no posting gets into it; and it is closed, so nothing posts in it any more. If verify
// fails, its error comes back and tx must roll back, which leaves the period open.
func (s *Service) ClosePeriod(ctx context.Context, tx postgres.Tx, legalEntity ids.ID, period, by string, verify func(ctx context.Context, from, to Date) error) error {
	return app.ClosePeriod(ctx, pg.NewStore(tx), legalEntity, period, by, verify)
}

// EnsureOpen is ErrPeriodClosed if d falls in a closed period of the entity: the ledger asks for it
// before posting. It reads without locking, like BusinessDate.
func (s *Service) EnsureOpen(ctx context.Context, q postgres.Querier, legalEntity ids.ID, d Date) error {
	return app.EnsureOpen(ctx, pg.NewStore(q), legalEntity, d)
}
