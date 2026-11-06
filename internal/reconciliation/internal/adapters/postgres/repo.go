// Package postgres runs the controls of reconciliation and stores their results in recon. The controls read
// ledger and calendar tables, read-only, in one statement each: a statement sees one snapshot, so a transaction
// in the middle of committing is never half seen.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/reconciliation/internal/app"
)

// statementTimeout replaces, within a control's transaction, the pool's statement_timeout: a control reads the
// whole journal.
const statementTimeout = "5min"

// errNoStatement means a control has no statement here.
var errNoStatement = errors.New("no statement")

// statements is the statement of each control: its rows are a Finding's entity, subject, currency and difference.
// An account with entries and no balance row counts as a balance of zero: the row went missing.
var statements = map[app.Control]string{
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

// Store is app.Recorder, without Publish, and what the controls read, within tx.
type Store struct{ tx postgres.Tx }

// NewStore is the store of reconciliation within tx.
func NewStore(tx postgres.Tx) Store { return Store{tx: tx} }

// Entities are the legal entities the controls go through.
func (s Store) Entities(ctx context.Context) ([]ids.ID, error) {
	rows, err := s.tx.Query(ctx, `SELECT id FROM calendar.legal_entities`)
	if err != nil {
		return nil, fmt.Errorf("read the legal entities: %w", err)
	}
	entities, err := pgx.CollectRows(rows, pgx.RowTo[ids.ID])
	if err != nil {
		return nil, fmt.Errorf("read the legal entities: %w", err)
	}
	return entities, nil
}

// Findings runs control c. One without a statement fails: an empty statement would pass, and leave a clean run
// of a control that looked at nothing.
func (s Store) Findings(ctx context.Context, c app.Control) ([]app.Finding, error) {
	statement, ok := statements[c]
	if !ok {
		return nil, fmt.Errorf("control %s: %w", c, errNoStatement)
	}
	if _, err := s.tx.Exec(ctx, `SELECT set_config('statement_timeout', $1, true)`, statementTimeout); err != nil {
		return nil, fmt.Errorf("control %s: %w", c, err)
	}
	rows, err := s.tx.Query(ctx, statement)
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
		cur, err := money.CurrencyOf(currency)
		if err != nil {
			return app.Finding{}, err
		}
		f.Difference = money.New(units, cur)
		return f, nil
	})
	if err != nil {
		return nil, fmt.Errorf("control %s: %w", c, err)
	}
	return found, nil
}

// LastSubjects are the subjects the entity's last run of c kept; none if it never ran.
func (s Store) LastSubjects(ctx context.Context, entity ids.ID, c app.Control) ([]string, error) {
	var subjects []string
	err := s.tx.QueryRow(ctx, `SELECT subjects FROM recon.control_runs WHERE legal_entity_id = $1 AND control = $2
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
	_, err := s.tx.Exec(ctx, `INSERT INTO recon.control_runs (id, legal_entity_id, control, findings, subjects)
		VALUES ($1, $2, $3, $4, coalesce($5::text[], '{}'))`, r.ID, r.LegalEntityID, r.Control, r.Findings, r.Subjects)
	if err != nil {
		return fmt.Errorf("store the run of %s: %w", r.Control, err)
	}
	return nil
}

// OpenBreak stores b and reports whether it is new: false if its entity, control and subject have one open.
func (s Store) OpenBreak(ctx context.Context, b app.Break) (bool, error) {
	tag, err := s.tx.Exec(ctx, `INSERT INTO recon.breaks (id, legal_entity_id, control, subject, currency, difference_units)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (legal_entity_id, control, subject) WHERE status = 'open' DO NOTHING`,
		b.ID, b.LegalEntityID, b.Control, b.Subject, b.Difference.Currency().Code(), b.Difference.Units())
	if err != nil {
		return false, fmt.Errorf("open a break of %s: %w", b.Control, err)
	}
	return tag.RowsAffected() == 1, nil
}
