package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/DrewGGM/aluna/internal/accounting/internal/domain"
	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/ids"
)

// Journal reads a period's journal.
type Journal interface {
	// UnbalancedCurrencies are the currencies whose entries of the entity, from from to to (excluded), do not sum
	// zero.
	UnbalancedCurrencies(ctx context.Context, entity ids.ID, from, to calendar.Date) ([]string, error)
}

// VerifyClose checks that a period of the entity, from from to to (excluded), can close (FR-37): its journal sums zero
// in every currency, and no reconciliation break of the entity is open, which anyOpenBreak answers. Otherwise
// domain.ErrPeriodNotSquare or domain.ErrOpenBreaks.
func VerifyClose(ctx context.Context, j Journal, entity ids.ID, from, to calendar.Date, anyOpenBreak func(context.Context) (bool, error)) error {
	unbalanced, err := j.UnbalancedCurrencies(ctx, entity, from, to)
	if err != nil {
		return err
	}
	if len(unbalanced) > 0 {
		return fmt.Errorf("%w in %s", domain.ErrPeriodNotSquare, strings.Join(unbalanced, ", "))
	}
	open, err := anyOpenBreak(ctx)
	if err != nil {
		return err
	}
	if open {
		return domain.ErrOpenBreaks
	}
	return nil
}
