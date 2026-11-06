// Package postgres runs the controls of reconciliation and keeps their results and the rail's statements in recon.
// The controls read ledger and calendar tables, read-only, in one query each: a query sees one snapshot, so a
// transaction in the middle of committing is never half seen. An import reads payments.orders, read-only too.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/reconciliation/internal/app"
)

// queryTimeout replaces, within a control's transaction, the pool's statement_timeout: a control reads the
// whole journal.
const queryTimeout = "5min"

// errNoQuery means a control has no query here.
var errNoQuery = errors.New("no query")

// controlQueries is the query of each control: its rows are a Finding's entity, subject, currency and difference.
// An account with entries and no balance row counts as a balance of zero: the row went missing.
var controlQueries = map[app.Control]string{
	app.TrialBalance: `SELECT legal_entity_id, currency::text, currency::text, sum(amount)::bigint
		FROM ledger.entries
		GROUP BY legal_entity_id, currency
		HAVING sum(amount) <> 0`,

	// An async account's balance reaches as far as the aggregator's cursor, and only that far of its entries
	// counts (migration 0005).
	app.BalancesMatchEntries: `WITH totals AS (
			SELECT e.account_id, sum(e.amount)::bigint AS total
			FROM ledger.entries e
			JOIN ledger.accounts a ON a.id = e.account_id
			JOIN ledger.transactions t ON t.id = e.transaction_id
			CROSS JOIN ledger.async_cursor c
			WHERE a.balance_mode = 'sync' OR (t.posted_xact, e.id) <= (c.posted_xact, c.entry_id)
			GROUP BY e.account_id)
		SELECT a.legal_entity_id, a.id::text, a.currency::text, coalesce(b.posted, 0) - coalesce(s.total, 0)
		FROM ledger.accounts a
		LEFT JOIN ledger.balances b ON b.account_id = a.id
		LEFT JOIN totals s ON s.account_id = a.id
		WHERE coalesce(b.posted, 0) <> coalesce(s.total, 0)`,

	// The codes are the ones payments gives a rail's accounts (L.<KIND>.<RAIL>).
	app.RailAccountsAtZero: `SELECT a.legal_entity_id, a.id::text, a.currency::text, sum(e.amount)::bigint
		FROM ledger.accounts a
		JOIN calendar.legal_entities l ON l.id = a.legal_entity_id
		JOIN ledger.entries e ON e.account_id = a.id
		JOIN ledger.transactions t ON t.id = e.transaction_id
		WHERE (a.code LIKE 'L.CLEARING.%' OR a.code LIKE 'L.SUSPENSE.%') AND t.business_date < l.business_date
		GROUP BY a.legal_entity_id, a.id, a.currency
		HAVING sum(e.amount) <> 0`,

	// The code is the one deposits gives a customer's account.
	app.CustomersMatchControl: `WITH totals AS (
			SELECT account_id, sum(amount) AS total FROM ledger.entries GROUP BY account_id)
		SELECT a.legal_entity_id, a.currency::text, a.currency::text, (coalesce(sum(b.posted), 0) - coalesce(sum(s.total), 0))::bigint
		FROM ledger.accounts a
		LEFT JOIN ledger.balances b ON b.account_id = a.id
		LEFT JOIN totals s ON s.account_id = a.id
		WHERE a.code LIKE 'deposits/%'
		GROUP BY a.legal_entity_id, a.currency
		HAVING coalesce(sum(b.posted), 0) <> coalesce(sum(s.total), 0)`,
}

// Store is what the controls and the statements read and keep, without what they publish.
type Store struct{ q postgres.Querier }

// NewStore is the store of reconciliation over q, a Tx or, to read the breaks, the pool.
func NewStore(q postgres.Querier) Store { return Store{q: q} }

// Entities are the legal entities the controls go through.
func (s Store) Entities(ctx context.Context) ([]ids.ID, error) {
	rows, err := s.q.Query(ctx, `SELECT id FROM calendar.legal_entities`)
	if err != nil {
		return nil, fmt.Errorf("read the legal entities: %w", err)
	}
	entities, err := pgx.CollectRows(rows, pgx.RowTo[ids.ID])
	if err != nil {
		return nil, fmt.Errorf("read the legal entities: %w", err)
	}
	return entities, nil
}

// Findings runs control c. One without a query fails: an empty one would pass, and leave a clean run of a control
// that looked at nothing.
func (s Store) Findings(ctx context.Context, c app.Control) ([]app.Finding, error) {
	query, ok := controlQueries[c]
	if !ok {
		return nil, fmt.Errorf("control %s: %w", c, errNoQuery)
	}
	if _, err := s.q.Exec(ctx, `SELECT set_config('statement_timeout', $1, true)`, queryTimeout); err != nil {
		return nil, fmt.Errorf("control %s: %w", c, err)
	}
	rows, err := s.q.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("control %s: %w", c, err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (app.Finding, error) {
		var f app.Finding
		var currency string
		var units int64
		if err := row.Scan(&f.LegalEntityID, &f.Subject, &currency, &units); err != nil {
			return app.Finding{}, err
		}
		var err error
		f.Difference, err = amountOf(units, currency)
		return f, err
	})
	if err != nil {
		return nil, fmt.Errorf("control %s: %w", c, err)
	}
	return found, nil
}

// LastSubjects are the subjects the entity's last run of c kept; none if it never ran.
func (s Store) LastSubjects(ctx context.Context, entity ids.ID, c app.Control) ([]string, error) {
	var subjects []string
	err := s.q.QueryRow(ctx, `SELECT subjects FROM recon.control_runs WHERE legal_entity_id = $1 AND control = $2
		ORDER BY ran_at DESC LIMIT 1`, entity, c).Scan(&subjects)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the last run of %s: %w", c, err)
	}
	return subjects, nil
}

// RecordRun stores r.
func (s Store) RecordRun(ctx context.Context, r app.Run) error {
	_, err := s.q.Exec(ctx, `INSERT INTO recon.control_runs (id, legal_entity_id, control, findings, subjects)
		VALUES ($1, $2, $3, $4, coalesce($5::text[], '{}'))`, r.ID, r.LegalEntityID, r.Control, r.Findings, r.Subjects)
	if err != nil {
		return fmt.Errorf("store the run of %s: %w", r.Control, err)
	}
	return nil
}

// amountOf is units of the currency with that code.
func amountOf(units int64, currency string) (money.Amount, error) {
	c, err := money.CurrencyOf(currency)
	if err != nil {
		return money.Amount{}, err
	}
	return money.New(units, c), nil
}

// orNull is id, or null if it is zero.
func orNull(id ids.ID) *ids.ID {
	if id == (ids.ID{}) {
		return nil
	}
	return &id
}

// OpenBreak stores b and reports whether it is new: false if its entity, control and subject have one open.
func (s Store) OpenBreak(ctx context.Context, b app.Break) (bool, error) {
	tag, err := s.q.Exec(ctx, `INSERT INTO recon.breaks (id, legal_entity_id, control, subject, currency, difference_units, owner,
			statement_id, reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, nullif($9, ''))
		ON CONFLICT (legal_entity_id, control, subject) WHERE status = 'open' DO NOTHING`,
		b.ID, b.LegalEntityID, b.Control, b.Subject, b.Difference.Currency().Code(), b.Difference.Units(), b.Owner, orNull(b.StatementID), b.Reason)
	if err != nil {
		return false, fmt.Errorf("open a break of %s: %w", b.Control, err)
	}
	return tag.RowsAffected() == 1, nil
}

// RecordStatement stores st, or app.ErrDuplicateStatement if the entity already imported it.
func (s Store) RecordStatement(ctx context.Context, st app.Statement) error {
	tag, err := s.q.Exec(ctx, `INSERT INTO recon.statements (id, legal_entity_id, rail, statement_ref, statement_date, line_count, imported_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (legal_entity_id, rail, statement_ref) DO NOTHING`,
		st.ID, st.LegalEntityID, st.Rail, st.Ref, st.Date, len(st.Lines), st.ImportedBy)
	if err != nil {
		return fmt.Errorf("store statement %s: %w", st.Ref, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", app.ErrDuplicateStatement, st.Ref)
	}
	return nil
}

// OrdersOf are the orders of st's rail with the id of one of its lines, of any day, and those of st's day the
// rail should report: the ones that went out or came in. A day is one of America/Bogota, as the rail's. They come
// by id, so that the same import pairs the same way.
func (s Store) OrdersOf(ctx context.Context, st app.Statement) ([]app.Order, error) {
	e2e := make([]string, len(st.Lines))
	for i, l := range st.Lines {
		e2e[i] = l.EndToEndID
	}
	rows, err := s.q.Query(ctx, `SELECT direction, end_to_end_id, currency::text, amount_units, settled, moved AND of_the_day
		FROM (
			SELECT id, direction, end_to_end_id, currency, amount_units, status IN ('settled', 'suspended') AS settled,
				status IN ('sent', 'settled', 'suspended') AS moved, (created_at AT TIME ZONE 'America/Bogota')::date = $3 AS of_the_day
			FROM payments.orders WHERE legal_entity_id = $1 AND rail = $2) o
		WHERE end_to_end_id = ANY($4) OR (moved AND of_the_day)
		ORDER BY id`, st.LegalEntityID, st.Rail, st.Date, e2e)
	if err != nil {
		return nil, fmt.Errorf("read the orders of statement %s: %w", st.Ref, err)
	}
	orders, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (app.Order, error) {
		var o app.Order
		var currency string
		var units int64
		if err := row.Scan(&o.Direction, &o.EndToEndID, &currency, &units, &o.Settled, &o.Expected); err != nil {
			return app.Order{}, err
		}
		var err error
		o.Amount, err = amountOf(units, currency)
		return o, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the orders of statement %s: %w", st.Ref, err)
	}
	return orders, nil
}

const selectBreak = `SELECT id, legal_entity_id, control, subject, coalesce(reason, ''), currency::text, difference_units, owner,
	statement_id, status, opened_at, cause, resolved_by, resolved_at
	FROM recon.breaks`

func scanBreak(row pgx.Row) (app.Break, error) {
	var b app.Break
	var currency string
	var units int64
	var statement *ids.ID
	var cause, by *string
	var at *time.Time
	err := row.Scan(&b.ID, &b.LegalEntityID, &b.Control, &b.Subject, &b.Reason, &currency, &units, &b.Owner, &statement, &b.Status,
		&b.OpenedAt, &cause, &by, &at)
	if err != nil {
		return app.Break{}, err
	}
	if statement != nil {
		b.StatementID = *statement
	}
	if cause != nil && by != nil && at != nil {
		b.Resolution = &app.Resolution{Cause: *cause, By: *by, At: *at}
	}
	b.Difference, err = amountOf(units, currency)
	return b, err
}

// LockBreak locks and returns break id of the entity, or app.ErrBreakNotFound.
func (s Store) LockBreak(ctx context.Context, entity, id ids.ID) (app.Break, error) {
	b, err := scanBreak(s.q.QueryRow(ctx, selectBreak+` WHERE id = $1 AND legal_entity_id = $2 FOR UPDATE`, id, entity))
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Break{}, fmt.Errorf("%w: %s", app.ErrBreakNotFound, id)
	}
	if err != nil {
		return app.Break{}, fmt.Errorf("read break %s: %w", id, err)
	}
	return b, nil
}

// ResolveBreak stores b's resolution and returns when it took effect, which the database stamps.
func (s Store) ResolveBreak(ctx context.Context, b app.Break) (time.Time, error) {
	var at time.Time
	err := s.q.QueryRow(ctx, `UPDATE recon.breaks SET status = $2, cause = $3, resolved_by = $4 WHERE id = $1 RETURNING resolved_at`,
		b.ID, b.Status, b.Resolution.Cause, b.Resolution.By).Scan(&at)
	if err != nil {
		return time.Time{}, fmt.Errorf("resolve break %s: %w", b.ID, err)
	}
	return at, nil
}

// BreaksBefore are the first limit breaks of the entity with an id below before, or from the newest one if it is
// zero, those in status if it has one, newest first.
func (s Store) BreaksBefore(ctx context.Context, entity ids.ID, status app.BreakStatus, before ids.ID, limit int) ([]app.Break, error) {
	rows, err := s.q.Query(ctx, selectBreak+` WHERE legal_entity_id = $1 AND ($2 = '' OR status = $2)
		AND ($3::uuid IS NULL OR id < $3) ORDER BY id DESC LIMIT $4`, entity, string(status), orNull(before), limit)
	if err != nil {
		return nil, fmt.Errorf("read the breaks: %w", err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (app.Break, error) { return scanBreak(row) })
	if err != nil {
		return nil, fmt.Errorf("read the breaks: %w", err)
	}
	return found, nil
}
