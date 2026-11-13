package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/ids"
)

// UnbalancedCurrencies are the currencies whose entries of the entity, with a business date from from to to
// (excluded), do not sum zero. It reads the ledger, read-only.
func (s Store) UnbalancedCurrencies(ctx context.Context, entity ids.ID, from, to calendar.Date) ([]string, error) {
	rows, err := s.q.Query(ctx, `SELECT e.currency::text
		FROM ledger.transactions t
		JOIN ledger.entries e ON e.transaction_id = t.id
		WHERE t.legal_entity_id = $1 AND t.business_date >= $2 AND t.business_date < $3
		GROUP BY e.currency
		HAVING sum(e.amount) <> 0
		ORDER BY e.currency`, entity, from.Time(), to.Time())
	if err != nil {
		return nil, fmt.Errorf("read the trial balance: %w", err)
	}
	currencies, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the trial balance: %w", err)
	}
	return currencies, nil
}
