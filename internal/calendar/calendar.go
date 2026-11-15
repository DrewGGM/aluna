// Package calendar is the calendar module (ADR-0006): legal entities, their business date and business
// days.
package calendar

import (
	"context"
	"time"

	pg "github.com/DrewGGM/aluna/internal/calendar/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/calendar/internal/app"
	"github.com/DrewGGM/aluna/internal/calendar/internal/domain"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// Date is a calendar day, with no time or zone.
type Date = domain.Date

// ParseDate reads a date yyyy-mm-dd, as Date.String writes it, or ErrInvalidDate.
func ParseDate(s string) (Date, error) { return domain.ParseDate(s) }

// DateOf is the day of t in its own location; Date.Time goes back, to midnight UTC.
func DateOf(t time.Time) Date { return domain.DateOf(t) }

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
	ErrInvalidLegalEntity  = app.ErrInvalidLegalEntity
	ErrStaleBusinessDate   = app.ErrStaleBusinessDate
	ErrBusinessDateAhead   = app.ErrBusinessDateAhead
	ErrInvalidDate         = domain.ErrInvalidDate
	ErrPeriodClosed        = app.ErrPeriodClosed
	ErrInvalidPeriod       = app.ErrInvalidPeriod
	ErrPeriodNotEnded      = app.ErrPeriodNotEnded
)

// typeRolled is the event of a rollover (architecture §8).
const typeRolled = "aluna.calendar.business-date.rolled.v1"

// Events publishes calendar events; outbox.Outbox implements it.
type Events interface {
	Emit(ctx context.Context, tx postgres.Tx, e outbox.Event) error
}

// Service is the calendar facade. Each operation receives the connection or the transaction it runs in.
type Service struct {
	events Events
	now    func() time.Time
}

// New creates the calendar service; it publishes its events to events, and now is the clock the rollover checks
// the day against.
func New(events Events, now func() time.Time) *Service { return &Service{events: events, now: now} }

// LegalEntity is a legal entity to register: its ID, its name, its first business date and its functional currency.
type LegalEntity = app.LegalEntity

// CreateLegalEntity registers e over q; or ErrInvalidLegalEntity if its date is not a business day or is after the
// day it is at now in America/Bogota, its currency is unknown or its name is empty. Only the tables' owner can:
// aluna_app has no INSERT on the entities, so whoever operates the deployment registers them (aluna entity create),
// never the API.
func CreateLegalEntity(ctx context.Context, q postgres.Querier, e LegalEntity, now time.Time) error {
	return app.CreateLegalEntity(ctx, pg.NewStore(q), e, now)
}

// BusinessDate is the business date of the legal entity. A use case reads it when its transaction starts
// and uses it for everything it posts: the read does not block the rollover. q can be the pool or a Tx.
func (s *Service) BusinessDate(ctx context.Context, q postgres.Querier, legalEntity ids.ID) (Date, error) {
	return pg.NewStore(q).BusinessDate(ctx, legalEntity)
}

// Rollover advances, within tx, the entity's business date from expected to the next business day, emits
// aluna.calendar.business-date.rolled.v1 and returns the new one. If the business date is not expected (another
// rollover advanced it, or the caller is behind), it is ErrStaleBusinessDate; if expected has not started yet in
// America/Bogota, ErrBusinessDateAhead. Either way nothing changes.
func (s *Service) Rollover(ctx context.Context, tx postgres.Tx, legalEntity ids.ID, expected Date) (Date, error) {
	next, err := app.Rollover(ctx, pg.NewStore(tx), legalEntity, expected, s.now())
	if err != nil {
		return Date{}, err
	}
	err = s.events.Emit(ctx, tx, outbox.Event{
		Type: typeRolled, Subject: "business-dates/" + legalEntity.String(), LegalEntityID: legalEntity, SchemaVersion: 1,
		Data: rolledPayload{ClosedBusinessDate: expected.String(), BusinessDate: next.String()},
	})
	if err != nil {
		return Date{}, err
	}
	return next, nil
}

// rolledPayload carries the day the rollover closed, which the end-of-day processes run on, and the new business
// date; the entity travels in the envelope.
type rolledPayload struct {
	ClosedBusinessDate string `json:"closed_business_date"`
	BusinessDate       string `json:"business_date"`
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

// EnsureOpen is ErrPeriodClosed if d falls in the entity's last closed period or before it (app.EnsureOpen): the
// ledger asks for it before posting. It reads without locking, like BusinessDate.
func (s *Service) EnsureOpen(ctx context.Context, q postgres.Querier, legalEntity ids.ID, d Date) error {
	return app.EnsureOpen(ctx, pg.NewStore(q), legalEntity, d)
}
