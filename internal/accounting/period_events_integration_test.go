//go:build integration

package accounting_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// TestAClosedPeriodIsAnEvent (architecture §8): closing a period emits aluna.accounting.period.closed.v1 about the
// entity's period, in the close's transaction, without who closed it: a close that rolls back leaves no event, and
// one that fails emits none.
func TestAClosedPeriodIsAnEvent(t *testing.T) {
	w := newCloseWorld(t)
	errUndo := errors.New("undo")
	undone := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		if err := w.s.ClosePeriod(t.Context(), tx, w.entity, "2026-02", "finance-b", w.anyOpenBreak(tx)); err != nil {
			return err
		}
		return errUndo
	})
	if !errors.Is(undone, errUndo) {
		t.Fatalf("close and roll back = %v, want the rollback", undone)
	}
	if err := w.close(t, "2026-03"); !errors.Is(err, calendar.ErrPeriodNotEnded) {
		t.Fatalf("closing the business month = %v, want ErrPeriodNotEnded", err)
	}

	if err := w.close(t, "2026-02"); err != nil {
		t.Fatal(err)
	}

	subject := "accounting-periods/" + w.entity.String() + "/"
	want := []pgtest.Event{{
		Type: "aluna.accounting.period.closed.v1", LegalEntityID: w.entity, SchemaVersion: 1, Data: map[string]any{"period": "2026-02"},
	}}
	if got := pgtest.EventsAbout(t, testPool, subject+"2026-02"); !reflect.DeepEqual(got, want) {
		t.Fatalf("events of 2026-02 = %+v, want %+v", got, want)
	}
	if got := pgtest.EventsAbout(t, testPool, subject+"2026-03"); len(got) != 0 {
		t.Fatalf("events of the failed close of 2026-03 = %+v, want none", got)
	}
}
