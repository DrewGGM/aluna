// Package postgres es el adapter de PostgreSQL del calendario.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/calendar/internal/adapters/postgres/sqlcgen"
	"github.com/DrewGGM/aluna/internal/calendar/internal/app"
	"github.com/DrewGGM/aluna/internal/calendar/internal/domain"
	"github.com/DrewGGM/aluna/internal/ids"
)

var (
	_ app.Store       = (*Store)(nil)
	_ app.PeriodStore = (*Store)(nil)
)

// Store implementa la persistencia del calendario sobre una conexión o una transacción.
type Store struct {
	q *sqlcgen.Queries
}

// NewStore crea el Store sobre db.
func NewStore(db sqlcgen.DBTX) *Store { return &Store{q: sqlcgen.New(db)} }

// BusinessDate lee la fecha de negocio de la entidad.
func (s *Store) BusinessDate(ctx context.Context, entity ids.ID) (domain.Date, error) {
	d, err := s.q.BusinessDate(ctx, entity)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Date{}, fmt.Errorf("%w: %s", app.ErrLegalEntityNotFound, entity)
	}
	if err != nil {
		return domain.Date{}, fmt.Errorf("leer la fecha de negocio: %w", err)
	}
	return domain.Date{Year: d.Year(), Month: d.Month(), Day: d.Day()}, nil
}

// AdvanceBusinessDate cambia la fecha de negocio de expected a next si sigue siendo expected.
func (s *Store) AdvanceBusinessDate(ctx context.Context, entity ids.ID, expected, next domain.Date) (bool, error) {
	n, err := s.q.AdvanceBusinessDate(ctx, sqlcgen.AdvanceBusinessDateParams{ID: entity, Expected: dateOf(expected), Next: dateOf(next)})
	if err != nil {
		return false, fmt.Errorf("avanzar la fecha de negocio: %w", err)
	}
	return n == 1, nil
}

// PeriodStatus lee el estado del periodo yyyy-mm de la entidad; found es false si no tiene fila.
func (s *Store) PeriodStatus(ctx context.Context, entity ids.ID, period string) (app.PeriodStatus, bool, error) {
	status, err := s.q.PeriodStatus(ctx, sqlcgen.PeriodStatusParams{LegalEntityID: entity, Period: period})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("leer el periodo %s: %w", period, err)
	}
	return app.PeriodStatus(status), true, nil
}

func dateOf(d domain.Date) time.Time { return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC) }
