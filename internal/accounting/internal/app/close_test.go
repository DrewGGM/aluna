package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/DrewGGM/aluna/internal/accounting/internal/app"
	"github.com/DrewGGM/aluna/internal/accounting/internal/domain"
	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/ids"
)

// journal is a Journal in memory with the currencies that do not sum zero.
type journal []string

func (j journal) UnbalancedCurrencies(context.Context, ids.ID, calendar.Date, calendar.Date) ([]string, error) {
	return j, nil
}

// TestAPeriodClosesSquareAndWithoutBreaks (FR-37): a journal that does not sum zero, or a break open, keep it from
// closing.
func TestAPeriodClosesSquareAndWithoutBreaks(t *testing.T) {
	for name, c := range map[string]struct {
		journal journal
		open    bool
		want    error
	}{
		"square, no breaks": {nil, false, nil},
		"not square":        {journal{"COP"}, true, domain.ErrPeriodNotSquare},
		"a break open":      {nil, true, domain.ErrOpenBreaks},
	} {
		anyOpenBreak := func(context.Context) (bool, error) { return c.open, nil }
		if err := app.VerifyClose(t.Context(), c.journal, ids.New(), calendar.Date{}, calendar.Date{}, anyOpenBreak); !errors.Is(err, c.want) {
			t.Errorf("%s: VerifyClose = %v, want %v", name, err, c.want)
		}
	}
}
