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

// entities is an EntityStore in memory.
type entities struct{ created []app.LegalEntity }

func (e *entities) CreateLegalEntity(_ context.Context, entity app.LegalEntity) error {
	e.created = append(e.created, entity)
	return nil
}

// TestALegalEntityStartsOnABusinessDay: an entity is registered with its name trimmed, a known functional currency
// and a business date that is a business day and has started in America/Bogota; anything else is
// ErrInvalidLegalEntity and nothing is stored. At 22:00 on Friday in Bogota, Friday is today and Monday is not yet.
func TestALegalEntityStartsOnABusinessDay(t *testing.T) {
	friday := domain.Date{Year: 2026, Month: time.October, Day: 30}
	now := time.Date(2026, time.October, 31, 3, 0, 0, 0, time.UTC)
	store := &entities{}
	if err := app.CreateLegalEntity(t.Context(), store, app.LegalEntity{ID: ids.New(), Name: " Entity ", BusinessDate: friday, Currency: "COP"}, now); err != nil ||
		len(store.created) != 1 || store.created[0].Name != "Entity" {
		t.Fatalf("CreateLegalEntity = %v, stored %+v; want the entity named Entity", err, store.created)
	}
	invalid := map[string]app.LegalEntity{
		"a Saturday":            {Name: "Entity", BusinessDate: domain.Date{Year: 2026, Month: time.October, Day: 31}, Currency: "COP"},
		"the next business day": {Name: "Entity", BusinessDate: domain.Date{Year: 2026, Month: time.November, Day: 3}, Currency: "COP"},
		"a holiday":             {Name: "Entity", BusinessDate: domain.Date{Year: 2026, Month: time.November, Day: 2}, Currency: "COP"},
		"an unknown currency":   {Name: "Entity", BusinessDate: friday, Currency: "XXX"},
		"a lowercase currency":  {Name: "Entity", BusinessDate: friday, Currency: "cop"},
		"a name of blanks":      {Name: "  ", BusinessDate: friday, Currency: "COP"},
	}
	for name, entity := range invalid {
		t.Run(name, func(t *testing.T) {
			store := &entities{}
			entity.ID = ids.New()

			if err := app.CreateLegalEntity(t.Context(), store, entity, now); !errors.Is(err, app.ErrInvalidLegalEntity) || len(store.created) != 0 {
				t.Fatalf("CreateLegalEntity = %v, stored %+v; want ErrInvalidLegalEntity and nothing stored", err, store.created)
			}
		})
	}
}
