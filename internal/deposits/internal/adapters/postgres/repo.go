// Package postgres stores deposit accounts in deposits.accounts, their status changes in
// deposits.status_changes and what they moved per day and month in deposits.counters.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/deposits/internal/app"
	"github.com/DrewGGM/aluna/internal/deposits/internal/domain"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// Store keeps deposit accounts over a connection or a transaction.
type Store struct{ q postgres.Querier }

// NewStore creates the store over q, the pool or a Tx.
func NewStore(q postgres.Querier) Store { return Store{q: q} }

// InsertAccount stores account a.
func (s Store) InsertAccount(ctx context.Context, a domain.Account) error {
	_, err := s.q.Exec(ctx, `INSERT INTO deposits.accounts (id, legal_entity_id, party_id, product_version_id, ledger_account_id,
			currency, status, opened_on, last_customer_activity_on)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		a.ID, a.LegalEntityID, a.PartyID, a.ProductVersionID, a.LedgerAccountID, a.Currency.Code(), a.Status,
		dateOf(a.OpenedOn), dateOf(a.LastCustomerActivityOn))
	if err != nil {
		return fmt.Errorf("insert deposit account %s: %w", a.ID, err)
	}
	return nil
}

const selectAccount = `SELECT id, legal_entity_id, party_id, product_version_id, ledger_account_id, currency, status,
		opened_on, last_customer_activity_on
	FROM deposits.accounts`

// Account returns account id of the entity, or app.ErrAccountNotFound.
func (s Store) Account(ctx context.Context, entity, id ids.ID) (domain.Account, error) {
	return scan(id, s.q.QueryRow(ctx, selectAccount+` WHERE id = $1 AND legal_entity_id = $2`, id, entity))
}

// LockAccount returns account id of the entity locked (FOR UPDATE) until the transaction ends, or
// app.ErrAccountNotFound. FOR UPDATE and not FOR SHARE: two movements holding a shared lock that both update
// the account later would deadlock upgrading it.
func (s Store) LockAccount(ctx context.Context, entity, id ids.ID) (domain.Account, error) {
	return scan(id, s.q.QueryRow(ctx, selectAccount+` WHERE id = $1 AND legal_entity_id = $2 FOR UPDATE`, id, entity))
}

// SetStatus moves account a to status to and records the change with its actor.
func (s Store) SetStatus(ctx context.Context, a domain.Account, to domain.Status, actor string) error {
	if _, err := s.q.Exec(ctx, `UPDATE deposits.accounts SET status = $2 WHERE id = $1`, a.ID, to); err != nil {
		return fmt.Errorf("move deposit account %s to %s: %w", a.ID, to, err)
	}
	_, err := s.q.Exec(ctx, `INSERT INTO deposits.status_changes (account_id, from_status, to_status, actor) VALUES ($1, $2, $3, $4)`,
		a.ID, a.Status, to, actor)
	if err != nil {
		return fmt.Errorf("record the status change of deposit account %s: %w", a.ID, err)
	}
	return nil
}

// RecordActivity sets the last customer activity of account id to on.
func (s Store) RecordActivity(ctx context.Context, id ids.ID, on calendar.Date) error {
	if _, err := s.q.Exec(ctx, `UPDATE deposits.accounts SET last_customer_activity_on = $2 WHERE id = $1`, id, dateOf(on)); err != nil {
		return fmt.Errorf("record the activity of deposit account %s: %w", id, err)
	}
	return nil
}

// Debits reads the debits of account id on day on and in its month, 0 for a period without movements.
func (s Store) Debits(ctx context.Context, id ids.ID, on calendar.Date) (daily, monthly int64, err error) {
	day, month := periods(on)
	err = s.q.QueryRow(ctx, `SELECT coalesce(sum(debits) FILTER (WHERE period = $2), 0), coalesce(sum(debits) FILTER (WHERE period = $3), 0)
		FROM deposits.counters WHERE account_id = $1 AND period IN ($2, $3)`, id, day, month).Scan(&daily, &monthly)
	if err != nil {
		return 0, 0, fmt.Errorf("read the counters of deposit account %s: %w", id, err)
	}
	return daily, monthly, nil
}

// Count adds debits and credits to the counters of account id of day on and of its month.
func (s Store) Count(ctx context.Context, id ids.ID, on calendar.Date, debits, credits int64) error {
	day, month := periods(on)
	_, err := s.q.Exec(ctx, `INSERT INTO deposits.counters (account_id, period, debits, credits) VALUES ($1, $2, $4, $5), ($1, $3, $4, $5)
		ON CONFLICT (account_id, period) DO UPDATE SET debits = counters.debits + EXCLUDED.debits, credits = counters.credits + EXCLUDED.credits`,
		id, day, month, debits, credits)
	if err != nil {
		return fmt.Errorf("count a movement of deposit account %s: %w", id, err)
	}
	return nil
}

// periods are the counters' periods of day on: the day (2026-03-02) and its month (2026-03).
func periods(on calendar.Date) (day, month string) {
	t := dateOf(on)
	return t.Format(time.DateOnly), t.Format("2006-01")
}

func scan(id ids.ID, row pgx.Row) (domain.Account, error) {
	var a domain.Account
	var currency string
	var openedOn, lastActivity time.Time
	err := row.Scan(&a.ID, &a.LegalEntityID, &a.PartyID, &a.ProductVersionID, &a.LedgerAccountID, &currency, &a.Status,
		&openedOn, &lastActivity)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Account{}, fmt.Errorf("%w: %s", app.ErrAccountNotFound, id)
	}
	if err != nil {
		return domain.Account{}, fmt.Errorf("read deposit account %s: %w", id, err)
	}
	if a.Currency, err = money.CurrencyOf(currency); err != nil {
		return domain.Account{}, fmt.Errorf("stored currency of deposit account %s: %w", id, err)
	}
	a.OpenedOn, a.LastCustomerActivityOn = calendarDate(openedOn), calendarDate(lastActivity)
	return a, nil
}

func dateOf(d calendar.Date) time.Time {
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC)
}

func calendarDate(t time.Time) calendar.Date {
	return calendar.Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}
}
