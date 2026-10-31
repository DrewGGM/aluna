package app

import (
	"context"
	"fmt"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/deposits/internal/domain/interest"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/products"
)

// DueAccount is an account due to accrue a day, with its accrual state at the end of the day before.
type DueAccount struct {
	ID, LedgerAccountID, ProductVersionID ids.ID
	State                                 interest.AccrualState
}

// Accrual is an account's accrual of a day: the balance it earned on, the day's interest and the state at the
// end of the day.
type Accrual struct {
	AccountID    ids.ID
	BalanceUnits int64
	Interest     money.Precise
	State        interest.AccrualState
}

// InterestTerms are what a product version sets for its interest.
type InterestTerms struct {
	Tiers    []products.RateTier
	DayCount products.DayCount
}

// Accruals is what accruing a day needs, within the transaction a batch runs in.
type Accruals interface {
	// Due locks (FOR UPDATE SKIP LOCKED: one being moved is left for a later run) up to batch accounts of the
	// entity, not closed, due to accrue day: opened on it or accrued up to the day before, and not accrued for
	// it yet.
	Due(ctx context.Context, entity ids.ID, day calendar.Date, batch int) ([]DueAccount, error)
	// ClosingBalance is the posted balance of a deposit's ledger account with a value date up to day: what it
	// held at the close of day.
	ClosingBalance(ctx context.Context, ledgerAccountID ids.ID, day calendar.Date) (int64, error)
	Terms(ctx context.Context, versionID ids.ID) (InterestTerms, error)
	InsertAccruals(ctx context.Context, day calendar.Date, rows []Accrual) error
}

// AccrueBatch accrues day for a batch of the entity's due accounts (FR-25) and returns how many it accrued:
// fewer than batch when no more are due (or the rest are locked). Each one accrues on its closing balance,
// from the state of the day before, with its product's tiers and day count; the rows are written once,
// keyed by account and value date, so running it again for the same day accrues nothing.
func AccrueBatch(ctx context.Context, s Accruals, entity ids.ID, day calendar.Date, batch int) (int, error) {
	due, err := s.Due(ctx, entity, day, batch)
	if err != nil || len(due) == 0 {
		return 0, err
	}
	terms := map[ids.ID]InterestTerms{}
	rows := make([]Accrual, 0, len(due))
	for _, a := range due {
		t, ok := terms[a.ProductVersionID]
		if !ok {
			if t, err = s.Terms(ctx, a.ProductVersionID); err != nil {
				return 0, err
			}
			terms[a.ProductVersionID] = t
		}
		balance, err := s.ClosingBalance(ctx, a.LedgerAccountID, day)
		if err != nil {
			return 0, err
		}
		state, earned, err := interest.AccrueDay(a.State, balance, t.Tiers, t.DayCount, day)
		if err != nil {
			return 0, fmt.Errorf("accrue deposit account %s: %w", a.ID, err)
		}
		rows = append(rows, Accrual{AccountID: a.ID, BalanceUnits: balance, Interest: earned, State: state})
	}
	if err := s.InsertAccruals(ctx, day, rows); err != nil {
		return 0, err
	}
	return len(rows), nil
}
