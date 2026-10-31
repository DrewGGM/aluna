// Package postgres stores deposit accounts in deposits.accounts, their status changes in
// deposits.status_changes, what they moved per day and month in deposits.counters and their daily interest
// in deposits.accruals.
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

// EntitiesToAccrue are the legal entities with deposit accounts that are not closed, in id order.
func (s Store) EntitiesToAccrue(ctx context.Context) ([]ids.ID, error) {
	rows, err := s.q.Query(ctx, `SELECT DISTINCT legal_entity_id FROM deposits.accounts WHERE status <> 'closed' ORDER BY legal_entity_id`)
	if err != nil {
		return nil, fmt.Errorf("read the entities with deposit accounts: %w", err)
	}
	entities, err := pgx.CollectRows(rows, pgx.RowTo[ids.ID])
	if err != nil {
		return nil, fmt.Errorf("read the entities with deposit accounts: %w", err)
	}
	return entities, nil
}

// DaysToAccrue are the closed days the entity has to accrue, in order: from the earliest one an account
// that is not closed has yet to accrue (the day after its last accrual, or its opening day) to the day before
// today, the entity's business date. None if every one is up to date.
func (s Store) DaysToAccrue(ctx context.Context, entity ids.ID, today calendar.Date) ([]calendar.Date, error) {
	rows, err := s.q.Query(ctx, `SELECT day::date FROM generate_series(
			(SELECT min(coalesce((SELECT max(r.value_date) FROM deposits.accruals r WHERE r.account_id = a.id) + 1, a.opened_on))
				FROM deposits.accounts a WHERE a.legal_entity_id = $1 AND a.status <> 'closed'),
			$2::date - 1, interval '1 day') AS day`, entity, dateOf(today))
	if err != nil {
		return nil, fmt.Errorf("find the days to accrue: %w", err)
	}
	days, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (calendar.Date, error) {
		var day time.Time
		err := row.Scan(&day)
		return calendarDate(day), err
	})
	if err != nil {
		return nil, fmt.Errorf("find the days to accrue: %w", err)
	}
	return days, nil
}

// Due locks, in id order and skipping those locked, up to batch accounts of the entity that are not closed
// and are due to accrue day, with their state at the end of the day before (app.Accruals).
func (s Store) Due(ctx context.Context, entity ids.ID, day calendar.Date, batch int) ([]app.DueAccount, error) {
	rows, err := s.q.Query(ctx, `SELECT a.id, a.ledger_account_id, a.product_version_id, coalesce(p.accrued_precise, 0)::text, coalesce(p.posted_units, 0)
		FROM deposits.accounts a
		LEFT JOIN deposits.accruals p ON p.account_id = a.id AND p.value_date = $2::date - 1
		WHERE a.legal_entity_id = $1 AND a.status <> 'closed' AND (p.account_id IS NOT NULL OR a.opened_on = $2)
			AND NOT EXISTS (SELECT 1 FROM deposits.accruals r WHERE r.account_id = a.id AND r.value_date = $2)
		ORDER BY a.id LIMIT $3
		FOR UPDATE OF a SKIP LOCKED`, entity, dateOf(day), batch)
	if err != nil {
		return nil, fmt.Errorf("read the accounts due to accrue %s: %w", dateOf(day).Format(time.DateOnly), err)
	}
	due, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (app.DueAccount, error) {
		var a app.DueAccount
		var accrued string
		if err := row.Scan(&a.ID, &a.LedgerAccountID, &a.ProductVersionID, &accrued, &a.State.PostedUnits); err != nil {
			return app.DueAccount{}, err
		}
		var err error
		a.State.AccruedPrecise, err = money.ParsePrecise(accrued)
		return a, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the accounts due to accrue %s: %w", dateOf(day).Format(time.DateOnly), err)
	}
	return due, nil
}

// InsertAccruals stores the accruals of day, in one statement.
func (s Store) InsertAccruals(ctx context.Context, day calendar.Date, rows []app.Accrual) error {
	accounts, balances, interest := make([]ids.ID, len(rows)), make([]int64, len(rows)), make([]string, len(rows))
	accrued, posted := make([]string, len(rows)), make([]int64, len(rows))
	for i, r := range rows {
		accounts[i], balances[i], interest[i] = r.AccountID, r.BalanceUnits, r.Interest.String()
		accrued[i], posted[i] = r.State.AccruedPrecise.String(), r.State.PostedUnits
	}
	_, err := s.q.Exec(ctx, `INSERT INTO deposits.accruals (account_id, value_date, balance_units, interest, accrued_precise, posted_units)
		SELECT account, $2, balance, interest::numeric, accrued::numeric, posted
		FROM unnest($1::uuid[], $3::bigint[], $4::text[], $5::text[], $6::bigint[]) AS r (account, balance, interest, accrued, posted)`,
		accounts, dateOf(day), balances, interest, accrued, posted)
	if err != nil {
		return fmt.Errorf("store the accruals of %s: %w", dateOf(day).Format(time.DateOnly), err)
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
