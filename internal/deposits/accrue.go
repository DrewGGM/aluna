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
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/outbox"
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

// accrueDay accrues day for the entity's due accounts in batches, in id order, each in its own transaction,
// until one comes out incomplete; each batch starts after the last account of the one before, so a day reads
// each account once.
func (s *Service) accrueDay(ctx context.Context, uow *postgres.UnitOfWork, entity ids.ID, day calendar.Date, batch int) (int, error) {
	accrued := 0
	var after ids.ID
	for {
		var n int
		var last ids.ID
		err := uow.Do(ctx, func(tx postgres.Tx) error { // a retried batch starts again from the same after
			var err error
			a := accruals{Store: pg.NewStore(tx), s: s, tx: tx}
			n, last, err = app.AccrueBatch(ctx, a, a, entity, day, after, batch)
			return err
		})
		if err != nil {
			return accrued, err
		}
		accrued, after = accrued+n, last
		if n < batch {
			return accrued, nil
		}
	}
}

// accruals is app.Accruals and app.Capitalizing over tx; the store gives Due and InsertAccruals.
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
	d := v.Definition
	return app.InterestTerms{
		Tiers: d.RateEA, DayCount: d.DayCount, Frequency: string(d.CapitalizationFrequency), Convention: d.DateConvention,
		ExpenseCode: d.Accounts.InterestExpense,
	}, nil
}

func (a accruals) AdjustDate(d calendar.Date, c calendar.Convention) calendar.Date {
	return a.s.calendar.AdjustDate(d, c)
}

func (a accruals) ExpenseAccount(ctx context.Context, entity ids.ID, code string, c money.Currency) (ids.ID, error) {
	return a.s.productAccount(ctx, a.tx, entity, "interest expense", code, c)
}

func (a accruals) Post(ctx context.Context, in ledger.PostInput) (ledger.Transaction, error) {
	return a.s.ledger.Post(ctx, a.tx, in)
}

func (a accruals) EmitCapitalized(ctx context.Context, entity ids.ID, e app.Capitalized) error {
	return a.s.events.Emit(ctx, a.tx, outbox.Event{
		Type: typeCapitalized, Subject: "deposit-accounts/" + e.AccountID.String(), LegalEntityID: entity, SchemaVersion: 1,
		Data: capitalizedPayload{
			AccountID: e.AccountID, Period: e.Period, TransactionID: e.TransactionID,
			ValueDate: time.Date(e.ValueDate.Year, e.ValueDate.Month, e.ValueDate.Day, 0, 0, 0, 0, time.UTC).Format(time.DateOnly),
			Amount:    e.Amount.Units(), Currency: e.Amount.Currency().Code(),
		},
	})
}

type capitalizedPayload struct {
	AccountID     ids.ID `json:"account_id"`
	Period        string `json:"period"`
	TransactionID ids.ID `json:"transaction_id"`
	ValueDate     string `json:"value_date"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
}
