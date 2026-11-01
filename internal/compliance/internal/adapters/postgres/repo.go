// Package postgres stores the screening cases in compliance.cases.
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

// selectCase reads a case of an entity by id; another entity's case looks the same as a missing one.
const selectCase = `SELECT kind, subject_id, status, coalesce(vendor_ref, ''), created_at, coalesce(resolved_by, '')
	FROM compliance.cases WHERE id = $1 AND legal_entity_id = $2`

// Case reads case id of entity.
func (s Store) Case(ctx context.Context, entity, id ids.ID) (app.Case, error) {
	return scan(s.q.QueryRow(ctx, selectCase, id, entity), entity, id)
}

// LockCase reads case id of entity FOR UPDATE.
func (s Store) LockCase(ctx context.Context, entity, id ids.ID) (app.Case, error) {
	return scan(s.q.QueryRow(ctx, selectCase+` FOR UPDATE`, id, entity), entity, id)
}

func scan(row pgx.Row, entity, id ids.ID) (app.Case, error) {
	c := app.Case{ID: id, LegalEntityID: entity}
	err := row.Scan(&c.Kind, &c.SubjectID, &c.Status, &c.VendorRef, &c.CreatedAt, &c.ResolvedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Case{}, fmt.Errorf("%w: %s", app.ErrCaseNotFound, id)
	}
	if err != nil {
		return app.Case{}, fmt.Errorf("read screening case %s: %w", id, err)
	}
	return c, nil
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
