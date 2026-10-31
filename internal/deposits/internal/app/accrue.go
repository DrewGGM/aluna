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
	Currency                              money.Currency
	State                                 interest.AccrualState
}

// Accrual is an account's accrual of a day: the balance it earned on, the day's interest and the state at the
// end of the day, capitalization included.
type Accrual struct {
	AccountID    ids.ID
	BalanceUnits int64
	Interest     money.Precise
	State        interest.AccrualState
}

// InterestTerms are what a product version sets for its interest: how it accrues, how often (Frequency, as
// the definition names it) and on which business day (Convention) it is capitalized, and against which
// ledger account (ExpenseCode).
type InterestTerms struct {
	Tiers       []products.RateTier
	DayCount    products.DayCount
	Frequency   string
	Convention  calendar.Convention
	ExpenseCode string
}

// Accruals is what accruing a day needs, within the transaction a batch runs in.
type Accruals interface {
	// Due locks (FOR UPDATE SKIP LOCKED: one being moved is left for a later run), in id order after after, up
	// to batch accounts of the entity, not closed, due to accrue day: opened on it or accrued up to the day
	// before, and not accrued for it yet.
	Due(ctx context.Context, entity ids.ID, day calendar.Date, after ids.ID, batch int) ([]DueAccount, error)
	// ClosingBalance is the posted balance of a deposit's ledger account with a value date up to day: what it
	// held at the close of day.
	ClosingBalance(ctx context.Context, ledgerAccountID ids.ID, day calendar.Date) (int64, error)
	Terms(ctx context.Context, versionID ids.ID) (InterestTerms, error)
	InsertAccruals(ctx context.Context, day calendar.Date, rows []Accrual) error
}

// AccrueBatch accrues day for a batch of the entity's due accounts with an id after after (FR-25) and, if day
// is the capitalization date of an account's product, capitalizes it (FR-26). It returns how many it accrued
// (fewer than batch when no more are due or the rest are locked) and the last one's id, where the next batch
// starts. Each one accrues on its closing balance, from the state of the day before, with its product's tiers
// and day count; the rows are written once, keyed by account and value date, so running it again for the same
// day accrues and capitalizes nothing.
func AccrueBatch(ctx context.Context, s Accruals, c Capitalizing, entity ids.ID, day calendar.Date, after ids.ID, batch int) (int, ids.ID, error) {
	due, err := s.Due(ctx, entity, day, after, batch)
	if err != nil || len(due) == 0 {
		return 0, after, err
	}
	versions := map[ids.ID]productDay{}
	rows := make([]Accrual, 0, len(due))
	for _, a := range due {
		p, ok := versions[a.ProductVersionID]
		if !ok {
			if p, err = productDayOf(ctx, s, c, a.ProductVersionID, day); err != nil {
				return 0, after, err
			}
			versions[a.ProductVersionID] = p
		}
		row, err := accrueAccount(ctx, s, c, entity, day, a, p)
		if err != nil {
			return 0, after, fmt.Errorf("deposit account %s: %w", a.ID, err)
		}
		rows = append(rows, row)
	}
	if err := s.InsertAccruals(ctx, day, rows); err != nil {
		return 0, after, err
	}
	return len(rows), due[len(due)-1].ID, nil
}

// productDay is what a product version sets for the accrual of a day: its interest terms and, if the day is
// its capitalization date, what to capitalize. It is the same for every account of the version.
type productDay struct {
	terms          InterestTerms
	capitalization capitalization
	capitalizes    bool
}

func productDayOf(ctx context.Context, s Accruals, c Capitalizing, versionID ids.ID, day calendar.Date) (productDay, error) {
	t, err := s.Terms(ctx, versionID)
	if err != nil {
		return productDay{}, err
	}
	period, capitalizes, err := capitalizationOn(day, t.Frequency, t.Convention, c.AdjustDate)
	if err != nil {
		return productDay{}, fmt.Errorf("product version %s: %w", versionID, err)
	}
	return productDay{terms: t, capitalization: capitalization{day, period, t.ExpenseCode}, capitalizes: capitalizes}, nil
}

// accrueAccount accrues day for account a and, on its product's capitalization date, capitalizes what it
// accrued.
func accrueAccount(ctx context.Context, s Accruals, c Capitalizing, entity ids.ID, day calendar.Date, a DueAccount, p productDay) (Accrual, error) {
	balance, err := s.ClosingBalance(ctx, a.LedgerAccountID, day)
	if err != nil {
		return Accrual{}, err
	}
	state, earned, err := interest.AccrueDay(a.State, balance, p.terms.Tiers, p.terms.DayCount, day)
	if err != nil {
		return Accrual{}, err
	}
	if p.capitalizes {
		if state, err = capitalizeInterest(ctx, c, entity, p.capitalization, a, state); err != nil {
			return Accrual{}, err
		}
	}
	return Accrual{AccountID: a.ID, BalanceUnits: balance, Interest: earned, State: state}, nil
}
