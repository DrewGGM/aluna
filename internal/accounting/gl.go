package accounting

import (
	"context"
	"errors"

	pg "github.com/DrewGGM/aluna/internal/accounting/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/accounting/internal/app"
	"github.com/DrewGGM/aluna/internal/accounting/internal/domain"
	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// Types of the daily GL.
type (
	// Line is a line of the daily GL: what the accounts of a CUIF code moved in a currency.
	Line = domain.Line
	// DailyGL is the GL of a day: when it was projected and its lines.
	DailyGL = domain.DailyGL
)

// ProjectGL projects the GL of every closed day that needs it, the newest first, each in its own transaction: one
// with no projection, or one whose projection may have missed a transaction that committed after it (FR-36,
// ADR-0011). Only the entities that map, from their first mapping: a mapping cannot start before the business date,
// so the days before it have no GL. A day that fails stops neither the others nor what they projected.
func (s *Service) ProjectGL(ctx context.Context, uow *postgres.UnitOfWork) error {
	var days []app.Day
	err := uow.Do(ctx, func(tx postgres.Tx) error {
		var err error
		days, err = pg.NewStore(tx).DaysToProject(ctx)
		return err
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, d := range days {
		errs = append(errs, uow.Do(ctx, func(tx postgres.Tx) error { return app.ProjectDay(ctx, pg.NewStore(tx), d) }))
	}
	return errors.Join(errs...)
}

// GL is the projected GL of the entity's day, whose lines sum zero by currency: ErrNotProjected if it has no
// projection, ErrDayUnmapped if an account of it had no mapping in force. q can be the pool or a Tx.
func (s *Service) GL(ctx context.Context, q postgres.Querier, entity ids.ID, day calendar.Date) (DailyGL, error) {
	return pg.NewStore(q).DailyGL(ctx, entity, day)
}
