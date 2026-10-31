// Package postgres stores deposit accounts in deposits.accounts and their status changes in
// deposits.status_changes.
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
