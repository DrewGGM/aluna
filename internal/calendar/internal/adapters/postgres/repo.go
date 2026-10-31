// Package postgres is the calendar's PostgreSQL adapter.
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

// Store implements calendar persistence over a connection or a transaction.
type Store struct {
	q *sqlcgen.Queries
}

// NewStore creates the Store over db.
func NewStore(db sqlcgen.DBTX) *Store { return &Store{q: sqlcgen.New(db)} }

// BusinessDate reads the entity's business date.
func (s *Store) BusinessDate(ctx context.Context, entity ids.ID) (domain.Date, error) {
	d, err := s.q.BusinessDate(ctx, entity)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Date{}, fmt.Errorf("%w: %s", app.ErrLegalEntityNotFound, entity)
	}
	if err != nil {
		return domain.Date{}, fmt.Errorf("read business date: %w", err)
	}
	return domain.Date{Year: d.Year(), Month: d.Month(), Day: d.Day()}, nil
}

// AdvanceBusinessDate changes the business date from expected to next if it is still expected.
func (s *Store) AdvanceBusinessDate(ctx context.Context, entity ids.ID, expected, next domain.Date) (bool, error) {
	n, err := s.q.AdvanceBusinessDate(ctx, sqlcgen.AdvanceBusinessDateParams{ID: entity, Expected: dateOf(expected), Next: dateOf(next)})
	if err != nil {
		return false, fmt.Errorf("advance business date: %w", err)
	}
	return n == 1, nil
}

// PeriodStatus reads the status of the entity's yyyy-mm period; found is false if it has no row.
func (s *Store) PeriodStatus(ctx context.Context, entity ids.ID, period string) (app.PeriodStatus, bool, error) {
	status, err := s.q.PeriodStatus(ctx, sqlcgen.PeriodStatusParams{LegalEntityID: entity, Period: period})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read period %s: %w", period, err)
	}
	return app.PeriodStatus(status), true, nil
}

func dateOf(d domain.Date) time.Time { return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC) }
