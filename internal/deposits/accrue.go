package deposits

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/DrewGGM/aluna/internal/calendar"
	pg "github.com/DrewGGM/aluna/internal/deposits/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/deposits/internal/app"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// Accrue accrues the interest of the closed business days of every entity (FR-25): for each one, in order,
// the days from the earliest an account has yet to accrue up to the day before its business date, each in
// batches of batch accounts, each batch in its own transaction. A day an account already accrued is not
// accrued again, so running it again is harmless. An entity that fails does not stop the others: their errors
// come back together. It returns how many account-days it accrued, counting only committed batches.
func (s *Service) Accrue(ctx context.Context, uow *postgres.UnitOfWork, batch int) (int, error) {
	if batch < 1 {
		return 0, fmt.Errorf("accrual batch %d: must be positive", batch)
	}
	var entities []ids.ID
	if err := uow.Do(ctx, func(tx postgres.Tx) error {
		var err error
		entities, err = pg.NewStore(tx).EntitiesToAccrue(ctx)
		return err
	}); err != nil {
		return 0, err
	}
	accrued := 0
	var errs []error
	for _, entity := range entities {
		n, err := s.accrueEntity(ctx, uow, entity, batch)
		accrued += n
		if err != nil {
			errs = append(errs, fmt.Errorf("accrue entity %s: %w", entity, err))
		}
	}
	return accrued, errors.Join(errs...)
}

// accrueEntity accrues the entity's closed days, in order, stopping at the first that fails: each day starts
// from the one before.
func (s *Service) accrueEntity(ctx context.Context, uow *postgres.UnitOfWork, entity ids.ID, batch int) (int, error) {
	var days []calendar.Date
	if err := uow.Do(ctx, func(tx postgres.Tx) error {
		today, err := s.calendar.BusinessDate(ctx, tx, entity)
		if err != nil {
			return err
		}
		days, err = pg.NewStore(tx).DaysToAccrue(ctx, entity, today)
		return err
	}); err != nil {
		return 0, err
	}
	accrued := 0
	for _, day := range days {
		n, err := s.accrueDay(ctx, uow, entity, day, batch)
		accrued += n
		if err != nil {
			return accrued, fmt.Errorf("accrue %s: %w", time.Date(day.Year, day.Month, day.Day, 0, 0, 0, 0, time.UTC).Format(time.DateOnly), err)
		}
	}
	return accrued, nil
}

// accrueDay accrues day for the entity's due accounts in batches, each in its own transaction, until one
// comes out incomplete.
func (s *Service) accrueDay(ctx context.Context, uow *postgres.UnitOfWork, entity ids.ID, day calendar.Date, batch int) (int, error) {
	accrued := 0
	for {
		var n int
		err := uow.Do(ctx, func(tx postgres.Tx) error {
			var err error
			n, err = app.AccrueBatch(ctx, accruals{Store: pg.NewStore(tx), s: s, tx: tx}, entity, day, batch)
			return err
		})
		if err != nil {
			return accrued, err
		}
		accrued += n
		if n < batch {
			return accrued, nil
		}
	}
}

// accruals is app.Accruals over tx; the store gives Due and InsertAccruals.
type accruals struct {
	pg.Store
	s  *Service
	tx postgres.Tx
}

func (a accruals) ClosingBalance(ctx context.Context, ledgerAccountID ids.ID, day calendar.Date) (int64, error) {
	valueDate := time.Date(day.Year, day.Month, day.Day, 0, 0, 0, 0, time.UTC)
	posted, err := a.s.ledger.PostedAsOf(ctx, a.tx, ledgerAccountID, ledger.BalanceAsOf{ValueDate: &valueDate})
	if err != nil {
		return 0, err
	}
	return ledger.NaturalBalance(ledger.KindLiability, posted), nil
}

func (a accruals) Terms(ctx context.Context, versionID ids.ID) (app.InterestTerms, error) {
	v, err := a.s.products.Version(ctx, a.tx, versionID)
	if err != nil {
		return app.InterestTerms{}, err
	}
	return app.InterestTerms{Tiers: v.Definition.RateEA, DayCount: v.Definition.DayCount}, nil
}
