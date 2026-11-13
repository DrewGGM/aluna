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
)

// ClosedPeriod is a closed period of an entity whose statements are not all generated yet.
type ClosedPeriod struct {
	Entity ids.ID
	Period string
}

// ClosedPeriods are the closed periods (calendar.periods) whose statements are not all generated, oldest first.
func (s Store) ClosedPeriods(ctx context.Context) ([]ClosedPeriod, error) {
	rows, err := s.q.Query(ctx, `SELECT p.legal_entity_id, p.period FROM calendar.periods p
		WHERE p.status = 'closed'
			AND NOT EXISTS (SELECT 1 FROM deposits.statement_periods d WHERE d.legal_entity_id = p.legal_entity_id AND d.period = p.period)
		ORDER BY p.period, p.legal_entity_id`)
	if err != nil {
		return nil, fmt.Errorf("read the closed periods without statements: %w", err)
	}
	periods, err := pgx.CollectRows(rows, pgx.RowToStructByPos[ClosedPeriod])
	if err != nil {
		return nil, fmt.Errorf("read the closed periods without statements: %w", err)
	}
	return periods, nil
}

// Pending are up to batch accounts of the entity, in id order after after, that period has to state and has not
// (app.Statements).
func (s Store) Pending(ctx context.Context, entity ids.ID, period string, from, to calendar.Date, after ids.ID, batch int) ([]domain.Account, error) {
	rows, err := s.q.Query(ctx, selectAccount+` a WHERE a.legal_entity_id = $1 AND a.opened_on < $4 AND a.id > $5
			AND NOT EXISTS (SELECT 1 FROM deposits.statements s WHERE s.account_id = a.id AND s.period = $2)
			AND (a.status <> 'closed' OR EXISTS (SELECT 1 FROM ledger.entries e JOIN ledger.transactions t ON t.id = e.transaction_id
				WHERE e.account_id = a.ledger_account_id AND t.business_date >= $3 AND t.business_date < $4))
		ORDER BY a.id LIMIT $6`, entity, period, dateOf(from), dateOf(to), after, batch)
	if err != nil {
		return nil, fmt.Errorf("read the accounts without statement: %w", err)
	}
	accounts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Account, error) { return scan(ids.ID{}, row) })
	if err != nil {
		return nil, fmt.Errorf("read the accounts without statement: %w", err)
	}
	return accounts, nil
}

// Month reads a's month from from to to, excluded, by business date: it reads the ledger, read-only. An entry's
// counter is the other one of its pair, in the order its transaction wrote them.
func (s Store) Month(ctx context.Context, a domain.Account, from, to calendar.Date) (app.Month, error) {
	var m app.Month
	if err := s.q.QueryRow(ctx, `SELECT coalesce(sum(e.amount), 0)::bigint FROM ledger.entries e JOIN ledger.transactions t ON t.id = e.transaction_id
		WHERE e.account_id = $1 AND t.business_date < $2`, a.LedgerAccountID, dateOf(from)).Scan(&m.Opening); err != nil {
		return app.Month{}, fmt.Errorf("read the opening balance of %s: %w", a.ID, err)
	}
	rows, err := s.q.Query(ctx, `WITH pairs AS (
			SELECT e.id, e.transaction_id, e.account_id, e.amount, e.posted_at,
				row_number() OVER (PARTITION BY e.transaction_id ORDER BY e.id) - 1 AS position
			FROM ledger.entries e
			WHERE e.transaction_id IN (SELECT m.transaction_id FROM ledger.entries m JOIN ledger.transactions t ON t.id = m.transaction_id
				WHERE m.account_id = $1 AND t.business_date >= $2 AND t.business_date < $3))
		SELECT t.id, t.code, t.business_date, t.value_date, e.posted_at, e.amount,
			coalesce(c.account_id, '00000000-0000-0000-0000-000000000000'), coalesce(c.amount, 0)
		FROM pairs e
		JOIN ledger.transactions t ON t.id = e.transaction_id
		LEFT JOIN pairs c ON c.transaction_id = e.transaction_id AND c.position = (e.position # 1)
		WHERE e.account_id = $1
		ORDER BY e.posted_at, e.id`, a.LedgerAccountID, dateOf(from), dateOf(to))
	if err != nil {
		return app.Month{}, fmt.Errorf("read the month of %s: %w", a.ID, err)
	}
	if m.Entries, err = pgx.CollectRows(rows, scanStatementEntry); err != nil {
		return app.Month{}, fmt.Errorf("read the month of %s: %w", a.ID, err)
	}
	return m, nil
}

func scanStatementEntry(row pgx.CollectableRow) (app.StatementEntry, error) {
	var e app.StatementEntry
	var business, value time.Time
	err := row.Scan(&e.TransactionID, &e.Code, &business, &value, &e.PostedAt, &e.Units, &e.Counter, &e.CounterUnits)
	e.BusinessDate, e.ValueDate = calendarDate(business), calendarDate(value)
	return e, err
}

// InsertStatement stores a's statement of period.
func (s Store) InsertStatement(ctx context.Context, a domain.Account, period string, st app.Statement) error {
	_, err := s.q.Exec(ctx, `INSERT INTO deposits.statements (account_id, period, document, content_hash) VALUES ($1, $2, $3, $4)`,
		a.ID, period, st.Document, st.ContentHash)
	if err != nil {
		return fmt.Errorf("store the statement of %s of %s: %w", a.ID, period, err)
	}
	return nil
}

// CompletePeriod records that the entity's period has every statement it has to.
func (s Store) CompletePeriod(ctx context.Context, entity ids.ID, period string) error {
	if _, err := s.q.Exec(ctx, `INSERT INTO deposits.statement_periods (legal_entity_id, period) VALUES ($1, $2)`, entity, period); err != nil {
		return fmt.Errorf("record the statements of %s as complete: %w", period, err)
	}
	return nil
}

// Statement reads account id's statement of period, or app.ErrStatementNotFound.
func (s Store) Statement(ctx context.Context, id ids.ID, period string) (app.Statement, error) {
	var st app.Statement
	err := s.q.QueryRow(ctx, `SELECT document, content_hash, generated_at FROM deposits.statements WHERE account_id = $1 AND period = $2`,
		id, period).Scan(&st.Document, &st.ContentHash, &st.GeneratedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Statement{}, fmt.Errorf("%w: %s of %s", app.ErrStatementNotFound, period, id)
	}
	if err != nil {
		return app.Statement{}, fmt.Errorf("read the statement of %s of %s: %w", id, period, err)
	}
	return st, nil
}
