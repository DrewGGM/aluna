package accounting

import (
	"context"

	pg "github.com/DrewGGM/aluna/internal/accounting/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/accounting/internal/app"
	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// CheckPeriodClose reports whether the entity's period, yyyy-mm, can close: calendar.ErrInvalidPeriod,
// calendar.ErrPeriodNotEnded or calendar.ErrPeriodClosed otherwise. q can be the pool or a Tx.
func (s *Service) CheckPeriodClose(ctx context.Context, q postgres.Querier, entity ids.ID, period string) error {
	return s.calendar.CheckPeriodClose(ctx, q, entity, period)
}

// typeClosed is the event of a closed period (architecture §8).
const typeClosed = "aluna.accounting.period.closed.v1"

// ClosePeriod closes the entity's period within tx, by who (FR-37): its trial balance must sum zero in every currency
// and no reconciliation break of the entity may be open, which anyOpenBreak answers (the breaks are
// reconciliation's), and then it is closed, nothing posts in it and aluna.accounting.period.closed.v1 is emitted.
// Otherwise ErrPeriodNotSquare or ErrOpenBreaks, besides the errors of calendar.ClosePeriod, and tx must roll back,
// which leaves the period open.
func (s *Service) ClosePeriod(ctx context.Context, tx postgres.Tx, entity ids.ID, period, by string, anyOpenBreak func(context.Context) (bool, error)) error {
	err := s.calendar.ClosePeriod(ctx, tx, entity, period, by, func(ctx context.Context, from, to calendar.Date) error {
		return app.VerifyClose(ctx, pg.NewStore(tx), entity, from, to, anyOpenBreak)
	})
	if err != nil {
		return err
	}
	return s.events.Emit(ctx, tx, outbox.Event{
		Type: typeClosed, Subject: "accounting-periods/" + entity.String() + "/" + period, LegalEntityID: entity, SchemaVersion: 1,
		Data: closedPayload{Period: period},
	})
}

// closedPayload leaves out who closed the period: every integrator receives the event, and a staff subject must not
// reach them; it stays in calendar.periods and the audit log.
type closedPayload struct {
	Period string `json:"period"`
}
