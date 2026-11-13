// Package postgres stores the screening cases in compliance.cases and the cash totals in compliance.cash_totals.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/compliance/internal/app"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// Store is app.Cases over q.
type Store struct{ q postgres.Querier }

// NewStore is the store of compliance over q, the pool or a Tx.
func NewStore(q postgres.Querier) Store { return Store{q: q} }

// InsertCase stores c; the database sets created_at. An empty vendor reference is stored as null.
func (s Store) InsertCase(ctx context.Context, c app.Case) (app.Case, error) {
	err := s.q.QueryRow(ctx, `INSERT INTO compliance.cases (id, legal_entity_id, kind, subject_id, status, vendor_ref)
		VALUES ($1, $2, $3, $4, $5, nullif($6, '')) RETURNING created_at`,
		c.ID, c.LegalEntityID, c.Kind, c.SubjectID, c.Status, c.VendorRef).Scan(&c.CreatedAt)
	if err != nil {
		return app.Case{}, fmt.Errorf("insert screening case: %w", err)
	}
	return c, nil
}

// fromCases reads the columns of a case that scanCase scans.
const fromCases = `SELECT id, kind, subject_id, status, coalesce(vendor_ref, ''), created_at, coalesce(resolved_by, '')
	FROM compliance.cases`

// selectCase reads a case of an entity by id; another entity's case looks the same as a missing one.
const selectCase = fromCases + ` WHERE id = $1 AND legal_entity_id = $2`

// Case reads case id of entity.
func (s Store) Case(ctx context.Context, entity, id ids.ID) (app.Case, error) {
	return scanOne(s.q.QueryRow(ctx, selectCase, id, entity), entity, id)
}

// LockCase reads case id of entity FOR UPDATE.
func (s Store) LockCase(ctx context.Context, entity, id ids.ID) (app.Case, error) {
	return scanOne(s.q.QueryRow(ctx, selectCase+` FOR UPDATE`, id, entity), entity, id)
}

func scanOne(row pgx.Row, entity, id ids.ID) (app.Case, error) {
	c, err := scanCase(row, entity)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Case{}, fmt.Errorf("%w: %s", app.ErrCaseNotFound, id)
	}
	if err != nil {
		return app.Case{}, fmt.Errorf("read screening case %s: %w", id, err)
	}
	return c, nil
}

func scanCase(row pgx.Row, entity ids.ID) (app.Case, error) {
	c := app.Case{LegalEntityID: entity}
	err := row.Scan(&c.ID, &c.Kind, &c.SubjectID, &c.Status, &c.VendorRef, &c.CreatedAt, &c.ResolvedBy)
	return c, err
}

// CasesBefore are the first limit cases of entity with an id below before, or from the newest if before is zero,
// those in status if it is not empty, newest first.
func (s Store) CasesBefore(ctx context.Context, entity ids.ID, status app.CaseStatus, before ids.ID, limit int) ([]app.Case, error) {
	var below *ids.ID // null reads from the newest
	if before != (ids.ID{}) {
		below = &before
	}
	rows, err := s.q.Query(ctx, fromCases+` WHERE legal_entity_id = $1 AND ($2 = '' OR status = $2)
		AND ($3::uuid IS NULL OR id < $3) ORDER BY id DESC LIMIT $4`, entity, string(status), below, limit)
	if err != nil {
		return nil, fmt.Errorf("read the screening cases: %w", err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (app.Case, error) { return scanCase(row, entity) })
	if err != nil {
		return nil, fmt.Errorf("read the screening cases: %w", err)
	}
	return found, nil
}

// ResolveCase records c's resolution: its status and who; the database stamps when.
func (s Store) ResolveCase(ctx context.Context, c app.Case) error {
	_, err := s.q.Exec(ctx, `UPDATE compliance.cases SET status = $2, resolved_by = $3 WHERE id = $1`,
		c.ID, c.Status, c.ResolvedBy)
	if err != nil {
		return fmt.Errorf("resolve screening case %s: %w", c.ID, err)
	}
	return nil
}

// AddCash adds m to its party's total of its month, direction and currency, which it creates the first time.
func (s Store) AddCash(ctx context.Context, m app.CashMovement) error {
	_, err := s.q.Exec(ctx, `INSERT INTO compliance.cash_totals (legal_entity_id, party_id, month, direction, currency, total_units)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (legal_entity_id, party_id, month, direction, currency)
		DO UPDATE SET total_units = cash_totals.total_units + EXCLUDED.total_units`,
		m.LegalEntityID, m.PartyID, m.Month, m.Direction, m.Amount.Currency().Code(), m.Amount.Units())
	if err != nil {
		return fmt.Errorf("add to the cash total of party %s: %w", m.PartyID, err)
	}
	return nil
}
