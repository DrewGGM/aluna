package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/products/internal/app"
)

// SQLSTATEs and constraints that reject an overlapping period: the exclusion constraint, or the primary key
// when both periods start on the same day (PostgreSQL checks it first).
const (
	exclusionViolation = "23P01"
	noOverlap          = "market_parameters_no_overlap"
	sameStart          = "market_parameters_pkey"
)

var _ app.MarketParameters = Store{}

// InsertParameter stores p. A period that overlaps another of the same kind and modality is
// app.ErrMarketParameterOverlap.
func (s Store) InsertParameter(ctx context.Context, p app.MarketParameter) error {
	_, err := s.q.Exec(ctx, `INSERT INTO products.market_parameters (kind, modality, valid_from, valid_to, value)
		VALUES ($1, $2, $3, $4, $5::numeric)`, p.Kind, p.Modality, dateOf(p.ValidFrom), dateOf(p.ValidTo), p.Value.String())
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == exclusionViolation && pgErr.ConstraintName == noOverlap ||
		pgErr.Code == uniqueViolation && pgErr.ConstraintName == sameStart) {
		return fmt.Errorf("%w: %s %s", app.ErrMarketParameterOverlap, p.Kind, p.Modality)
	}
	if err != nil {
		return fmt.Errorf("insert market parameter %s %s: %w", p.Kind, p.Modality, err)
	}
	return nil
}

// Overlaps reports whether another value of the same kind and modality holds on some day of p's period.
func (s Store) Overlaps(ctx context.Context, p app.MarketParameter) (bool, error) {
	var overlaps bool
	err := s.q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM products.market_parameters
		WHERE kind = $1 AND modality = $2 AND daterange(valid_from, valid_to) && daterange($3, $4))`,
		p.Kind, p.Modality, dateOf(p.ValidFrom), dateOf(p.ValidTo)).Scan(&overlaps)
	if err != nil {
		return false, fmt.Errorf("check the periods of market parameter %s %s: %w", p.Kind, p.Modality, err)
	}
	return overlaps, nil
}

// ValueOn is the value of the parameter that holds on day on, or app.ErrNoMarketParameter. The value is read
// as text, so it reaches money.Precise without going through a float.
func (s Store) ValueOn(ctx context.Context, kind app.MarketKind, modality string, on calendar.Date) (money.Precise, error) {
	var value string
	err := s.q.QueryRow(ctx, `SELECT value::text FROM products.market_parameters
		WHERE kind = $1 AND modality = $2 AND daterange(valid_from, valid_to) @> $3::date`, kind, modality, dateOf(on)).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return money.Precise{}, fmt.Errorf("%w: %s %s on %d-%02d-%02d", app.ErrNoMarketParameter, kind, modality, on.Year, on.Month, on.Day)
	}
	if err != nil {
		return money.Precise{}, fmt.Errorf("read market parameter %s %s: %w", kind, modality, err)
	}
	parsed, err := money.ParsePrecise(value)
	if err != nil {
		return money.Precise{}, fmt.Errorf("stored market parameter %s %s: %w", kind, modality, err)
	}
	return parsed, nil
}

func dateOf(d calendar.Date) time.Time {
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC)
}
