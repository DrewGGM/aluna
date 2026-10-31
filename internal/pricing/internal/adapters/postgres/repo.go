// Package postgres stores the quotes in pricing.quotes. Their times are the database's, the system's only
// clock (ADR-0006): the quote's expiry and its use are compared on the same one.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/pricing/internal/app"
	"github.com/DrewGGM/aluna/internal/pricing/internal/domain"
)

// Store keeps the quotes over a connection or a transaction.
type Store struct{ q postgres.Querier }

// NewStore creates the store over q, the pool or a Tx.
func NewStore(q postgres.Querier) Store { return Store{q: q} }

// storedFee is a fee as the quote keeps it, in the quote's currency.
type storedFee struct {
	Code  string `json:"code"`
	Units int64  `json:"units"`
}

// UsedThisMonth is how many quotes of account accountID of the entity were used in the current month, in
// America/Bogota: since its first instant, as no quote is used later than now. statement_timestamp(), stable
// within the query, lets the index search the range.
func (s Store) UsedThisMonth(ctx context.Context, entity, accountID ids.ID) (int, error) {
	var n int
	err := s.q.QueryRow(ctx, `SELECT count(*) FROM pricing.quotes WHERE account_id = $1 AND legal_entity_id = $2
		AND used_at >= date_trunc('month', statement_timestamp() AT TIME ZONE 'America/Bogota') AT TIME ZONE 'America/Bogota'`,
		accountID, entity).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count the quotes used this month: %w", err)
	}
	return n, nil
}

// InsertQuote stores q, which expires ttl after the database's now, and returns it with its times. created_at
// is statement_timestamp() too: read once, so expires_at is exactly ttl after it.
func (s Store) InsertQuote(ctx context.Context, q app.Quote, ttl time.Duration) (app.Quote, error) {
	fees := make([]storedFee, len(q.Fees))
	for i, f := range q.Fees {
		fees[i] = storedFee{Code: f.Code, Units: f.Amount.Units()}
	}
	document, err := json.Marshal(fees)
	if err != nil {
		return app.Quote{}, fmt.Errorf("quote fees: %w", err)
	}
	err = s.q.QueryRow(ctx, `INSERT INTO pricing.quotes (id, legal_entity_id, account_id, operation, currency, amount_units, fees, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, statement_timestamp() + $8::bigint * interval '1 microsecond')
		RETURNING created_at, expires_at`,
		q.ID, q.LegalEntityID, q.AccountID, q.Operation, q.Amount.Currency().Code(), q.Amount.Units(), document, ttl.Microseconds(),
	).Scan(&q.CreatedAt, &q.ExpiresAt)
	if err != nil {
		return app.Quote{}, fmt.Errorf("store quote %s: %w", q.ID, err)
	}
	return q, nil
}

// UseQuote marks as used the quote of the entity that u names, if it is of u's account, operation and amount,
// is not used and has not expired by the database's now, and returns its fees; otherwise it is
// app.ErrQuoteUnavailable.
func (s Store) UseQuote(ctx context.Context, entity ids.ID, u app.QuoteUse) ([]domain.Fee, error) {
	var document []byte
	err := s.q.QueryRow(ctx, `UPDATE pricing.quotes SET used_at = clock_timestamp()
		WHERE id = $1 AND legal_entity_id = $2 AND account_id = $3 AND operation = $4 AND currency = $5 AND amount_units = $6
			AND used_at IS NULL AND expires_at > clock_timestamp()
		RETURNING fees`,
		u.QuoteID, entity, u.AccountID, u.Operation, u.Amount.Currency().Code(), u.Amount.Units()).Scan(&document)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", app.ErrQuoteUnavailable, u.QuoteID)
	}
	if err != nil {
		return nil, fmt.Errorf("use quote %s: %w", u.QuoteID, err)
	}
	var stored []storedFee
	if err := json.Unmarshal(document, &stored); err != nil {
		return nil, fmt.Errorf("stored fees of quote %s: %w", u.QuoteID, err)
	}
	fees := make([]domain.Fee, len(stored))
	for i, f := range stored {
		fees[i] = domain.Fee{Code: f.Code, Amount: money.New(f.Units, u.Amount.Currency())}
	}
	return fees, nil
}
