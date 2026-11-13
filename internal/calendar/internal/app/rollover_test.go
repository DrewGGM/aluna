package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/calendar/internal/app"
	"github.com/DrewGGM/aluna/internal/calendar/internal/domain"
	"github.com/DrewGGM/aluna/internal/ids"
)

// businessDates is a Store in memory of one entity.
type businessDates struct{ today domain.Date }

func (b *businessDates) BusinessDate(context.Context, ids.ID) (domain.Date, error) {
	return b.today, nil
}

func (b *businessDates) AdvanceBusinessDate(_ context.Context, _ ids.ID, expected, next domain.Date) (bool, error) {
	if b.today != expected {
		return false, nil
	}
	b.today = next
	return true, nil
}

// TestTheBusinessDateDoesNotRunAheadOfTheCalendar: a day closes once it has started in America/Bogota, so the evening
// rollover of today goes ahead, and a business date already after today stays where it is. At 23:00 in Bogota it is
// still the previous day there, although it is the next one in UTC.
func TestTheBusinessDateDoesNotRunAheadOfTheCalendar(t *testing.T) {
	friday, tuesday := domain.Date{Year: 2026, Month: time.October, Day: 30}, domain.Date{Year: 2026, Month: time.November, Day: 3}
	cases := map[string]struct {
		now     time.Time
		want    domain.Date
		wantErr error
	}{
		"Friday evening in Bogota":     {time.Date(2026, time.October, 31, 1, 0, 0, 0, time.UTC), tuesday, nil},
		"a week later":                 {time.Date(2026, time.November, 6, 15, 0, 0, 0, time.UTC), tuesday, nil},
		"Thursday 23:00 in Bogota":     {time.Date(2026, time.October, 30, 4, 0, 0, 0, time.UTC), friday, app.ErrBusinessDateAhead},
		"Thursday afternoon in Bogota": {time.Date(2026, time.October, 29, 20, 0, 0, 0, time.UTC), friday, app.ErrBusinessDateAhead},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := &businessDates{today: friday}

			next, err := app.Rollover(t.Context(), store, ids.New(), friday, tc.now)

			if !errors.Is(err, tc.wantErr) || store.today != tc.want || (err == nil && next != tc.want) {
				t.Fatalf("Rollover = %v, %v with the business date on %v; want %v, %v", next, err, store.today, tc.want, tc.wantErr)
			}
		})
	}
}
