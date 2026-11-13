//go:build integration

package calendar_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// TestTheOwnerCreatesALegalEntity: the tables' owner registers an entity, which starts on its business date; one on a
// day that is not a business day is ErrInvalidLegalEntity, and aluna_app cannot register any (it has no INSERT).
func TestTheOwnerCreatesALegalEntity(t *testing.T) {
	friday := calendar.Date{Year: 2026, Month: time.October, Day: 30}
	entity := ids.New()
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		return calendar.CreateLegalEntity(t.Context(), tx, calendar.LegalEntity{ID: entity, Name: "Entity", BusinessDate: friday, Currency: "COP"}, time.Now())
	})

	if got := businessDate(t, entity); got != friday {
		t.Fatalf("business date = %v, want %v", got, friday)
	}
	saturday := calendar.Date{Year: 2026, Month: time.October, Day: 31}
	if err := calendar.CreateLegalEntity(t.Context(), testPool, calendar.LegalEntity{ID: ids.New(), Name: "Entity", BusinessDate: saturday, Currency: "COP"}, time.Now()); !errors.Is(err, calendar.ErrInvalidLegalEntity) {
		t.Fatalf("an entity on a Saturday = %v, want ErrInvalidLegalEntity", err)
	}
	var pgErr *pgconn.PgError
	if err := calendar.CreateLegalEntity(t.Context(), testPool, calendar.LegalEntity{ID: ids.New(), Name: "Entity", BusinessDate: friday, Currency: "COP"}, time.Now()); !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("aluna_app creating an entity = %v, want SQLSTATE 42501", err)
	}
}
