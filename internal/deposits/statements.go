package deposits

import (
	"context"
	"errors"
	"fmt"

	pg "github.com/DrewGGM/aluna/internal/deposits/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/deposits/internal/app"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/products"
)

// ErrStatementNotFound means an account without a statement of that month: its period is not closed yet or the
// statements job has not reached it, the account did not exist then, or it was closed and did not move in it.
var ErrStatementNotFound = app.ErrStatementNotFound

// Statement is an account's statement of a month (FR-45): its document, canonical JSON with the opening balance, the
// movements, the interest, the fees, the GMF and the closing balance; the SHA-256 of the document, in hex; and when
// it was generated.
type Statement = app.Statement

// GenerateStatements generates, in every entity, the statements of its closed periods that do not have them yet
// (FR-45): one per account opened before the period ended, unless it was closed and did not move in it. Each batch of
// batch accounts goes in its own transaction; once a period has every statement it is not read again. A period that
// fails does not stop the others: their errors come back together. It returns how many it generated, counting only
// committed batches.
func (s *Service) GenerateStatements(ctx context.Context, uow *postgres.UnitOfWork, batch int) (int, error) {
	if batch < 1 {
		return 0, fmt.Errorf("statements batch %d: must be positive", batch)
	}
	var periods []pg.ClosedPeriod
	if err := uow.Do(ctx, func(tx postgres.Tx) error {
		var err error
		periods, err = pg.NewStore(tx).ClosedPeriods(ctx)
		return err
	}); err != nil {
		return 0, err
	}
	generated := 0
	var errs []error
	for _, p := range periods {
		n, err := s.generatePeriod(ctx, uow, p, batch)
		generated += n
		if err != nil {
			errs = append(errs, fmt.Errorf("statements of %s in entity %s: %w", p.Period, p.Entity, err))
		}
	}
	return generated, errors.Join(errs...)
}

// generatePeriod generates the statements of a period in batches, in account id order, until one comes out
// incomplete.
func (s *Service) generatePeriod(ctx context.Context, uow *postgres.UnitOfWork, p pg.ClosedPeriod, batch int) (int, error) {
	generated := 0
	var after ids.ID
	for {
		var n int
		var last ids.ID
		err := uow.Do(ctx, func(tx postgres.Tx) error { // a retried batch starts again from the same after
			var err error
			st := statements{Store: pg.NewStore(tx), s: s, tx: tx, counters: map[ids.ID]app.CounterAccounts{}}
			n, last, err = app.StatementBatch(ctx, st, p.Entity, p.Period, after, batch)
			return err
		})
		if err != nil {
			return generated, err
		}
		generated, after = generated+n, last
		if n < batch {
			return generated, nil
		}
	}
}

// Statement reads the statement of period (yyyy-mm) of account id of the entity: ErrAccountNotFound, or
// ErrStatementNotFound if it has none. Reading it costs nothing (FR-45). q can be the pool or a Tx.
func (s *Service) Statement(ctx context.Context, q postgres.Querier, entity, id ids.ID, period string) (Statement, error) {
	store := pg.NewStore(q)
	if _, err := store.Account(ctx, entity, id); err != nil {
		return Statement{}, err
	}
	return store.Statement(ctx, id, period)
}

// statements is app.Statements over tx; the store gives the rest, and counters keeps a batch's counter accounts by
// product version, which they depend on alone.
type statements struct {
	pg.Store
	s        *Service
	tx       postgres.Tx
	counters map[ids.ID]app.CounterAccounts
}

// CounterAccounts are the accounts a's product names for its interest, its fees and its GMF. One that does not exist
// is the zero ID: nothing was ever posted on it, since a movement that needs it fails, so no entry of a's is of that
// kind.
func (st statements) CounterAccounts(ctx context.Context, a Account) (app.CounterAccounts, error) {
	if c, ok := st.counters[a.ProductVersionID]; ok {
		return c, nil
	}
	m := st.s.movements(st.tx)
	var c app.CounterAccounts
	for _, k := range []struct {
		role string
		code func(products.Version) string
		id   *ids.ID
	}{
		{"interest expense", func(v products.Version) string { return v.Definition.Accounts.InterestExpense }, &c.InterestExpense},
		{"fee income", func(v products.Version) string { return v.Definition.Accounts.FeeIncome }, &c.FeeIncome},
		{"GMF payable", func(v products.Version) string { return v.Definition.Accounts.GMFPayable }, &c.GMFPayable},
	} {
		id, err := m.productAccountOf(ctx, a, k.role, k.code)
		if err != nil && !errors.Is(err, ErrProductAccountMissing) {
			return app.CounterAccounts{}, err
		}
		*k.id = id
	}
	st.counters[a.ProductVersionID] = c
	return c, nil
}
