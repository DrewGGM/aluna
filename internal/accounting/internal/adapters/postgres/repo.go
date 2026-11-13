// Package postgres keeps accounting's CUIF mappings in accounting.cuif_mappings.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/DrewGGM/aluna/internal/accounting/internal/domain"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// Constraints that refuse an overlap: PostgreSQL checks the primary key before the exclusion, so two mappings of a
// pattern with the same start break the first.
const (
	noOverlap = "cuif_mappings_no_overlap"
	sameStart = "cuif_mappings_pkey"
)

// Store is app.Mappings over q.
type Store struct{ q postgres.Querier }

// NewStore is the store of accounting over q, the pool or a Tx.
func NewStore(q postgres.Querier) Store { return Store{q: q} }

// InsertMapping stores m: domain.ErrMappingOverlap if another of its pattern holds in its period.
func (s Store) InsertMapping(ctx context.Context, entity ids.ID, m domain.Mapping) error {
	_, err := s.q.Exec(ctx, `INSERT INTO accounting.cuif_mappings (legal_entity_id, pattern, cuif_code, valid_from, valid_to)
		VALUES ($1, $2, $3, $4, $5)`, entity, m.Pattern, m.CUIFCode, m.ValidFrom, m.ValidTo)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.ConstraintName == noOverlap || pgErr.ConstraintName == sameStart) {
		return fmt.Errorf("%w: pattern %s", domain.ErrMappingOverlap, m.Pattern)
	}
	if err != nil {
		return fmt.Errorf("insert the mapping of %s: %w", m.Pattern, err)
	}
	return nil
}

// Mappings are all the entity's mappings.
func (s Store) Mappings(ctx context.Context, entity ids.ID) ([]domain.Mapping, error) {
	rows, err := s.q.Query(ctx, `SELECT pattern, cuif_code, valid_from, valid_to FROM accounting.cuif_mappings WHERE legal_entity_id = $1`, entity)
	if err != nil {
		return nil, fmt.Errorf("read the mappings: %w", err)
	}
	mappings, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Mapping, error) {
		var m domain.Mapping
		err := row.Scan(&m.Pattern, &m.CUIFCode, &m.ValidFrom, &m.ValidTo)
		return m, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the mappings: %w", err)
	}
	return mappings, nil
}
