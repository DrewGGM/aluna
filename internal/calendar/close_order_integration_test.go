//go:build integration

package calendar_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// TestPeriodsCloseInOrderWhenAsked (Q-131 f, ADR-0020): with January closed, a calendar that closes in order refuses
// to check or close March (ErrPeriodOutOfOrder) and checks February; one that does not, checks March.
func TestPeriodsCloseInOrderWhenAsked(t *testing.T) {
	entity := newEntity(t, calendar.Date{Year: 2026, Month: time.April, Day: 6})
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO calendar.periods (legal_entity_id, period, status, closed_at, closed_by)
			VALUES ($1, '2026-01', 'closed', clock_timestamp(), 'finance')`, entity)
		return err
	})
	ordered := calendar.New(nil, time.Now).CloseInOrder()

	if err := ordered.CheckPeriodClose(t.Context(), testPool, entity, "2026-03"); !errors.Is(err, calendar.ErrPeriodOutOfOrder) {
		t.Errorf("checking March = %v, want ErrPeriodOutOfOrder", err)
	}
	err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		return ordered.ClosePeriod(t.Context(), tx, entity, "2026-03", "finance", nil)
	})
	if !errors.Is(err, calendar.ErrPeriodOutOfOrder) {
		t.Errorf("closing March = %v, want ErrPeriodOutOfOrder", err)
	}
	if err := ordered.CheckPeriodClose(t.Context(), testPool, entity, "2026-02"); err != nil {
		t.Errorf("checking February = %v, want nil", err)
	}
	if err := calendar.New(nil, time.Now).CheckPeriodClose(t.Context(), testPool, entity, "2026-03"); err != nil {
		t.Errorf("checking March in any order = %v, want nil", err)
	}
}
