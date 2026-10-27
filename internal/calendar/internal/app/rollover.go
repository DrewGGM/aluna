// Package app son los casos de uso del calendario.
package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/DrewGGM/aluna/internal/calendar/internal/domain"
	"github.com/DrewGGM/aluna/internal/ids"
)

var (
	// ErrLegalEntityNotFound indica una entidad legal que no está registrada.
	ErrLegalEntityNotFound = errors.New("entidad legal no encontrada")
	// ErrStaleBusinessDate indica un rollover cuya fecha esperada ya no es la fecha de negocio: otro
	// rollover la avanzó.
	ErrStaleBusinessDate = errors.New("la fecha de negocio ya no es la esperada")
)

// Store es la persistencia de la fecha de negocio de cada entidad legal.
type Store interface {
	// BusinessDate lee la fecha de negocio de la entidad, o ErrLegalEntityNotFound.
	BusinessDate(ctx context.Context, entity ids.ID) (domain.Date, error)
	// AdvanceBusinessDate la cambia de expected a next solo si sigue siendo expected, y dice si cambió.
	AdvanceBusinessDate(ctx context.Context, entity ids.ID, expected, next domain.Date) (bool, error)
}

// Rollover avanza la fecha de negocio de la entidad de expected al día hábil siguiente (FR-13). Es un
// UPDATE condicional: no bloquea a los postings que ya leyeron la fecha, y un segundo rollover con la
// misma expected es ErrStaleBusinessDate en vez de avanzar dos días.
func Rollover(ctx context.Context, store Store, entity ids.ID, expected domain.Date) (domain.Date, error) {
	next := domain.Adjust(dayAfter(expected), domain.Following, isColombianHoliday)
	advanced, err := store.AdvanceBusinessDate(ctx, entity, expected, next)
	if err != nil {
		return domain.Date{}, err
	}
	if advanced {
		return next, nil
	}
	current, err := store.BusinessDate(ctx, entity)
	if err != nil {
		return domain.Date{}, err
	}
	return domain.Date{}, fmt.Errorf("%w: es %s, se esperaba %s", ErrStaleBusinessDate, iso(current), iso(expected))
}

func iso(d domain.Date) string { return fmt.Sprintf("%04d-%02d-%02d", d.Year, d.Month, d.Day) }

func dayAfter(d domain.Date) domain.Date {
	t := time.Date(d.Year, d.Month, d.Day+1, 0, 0, 0, 0, time.UTC)
	return domain.Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}
}

// isColombianHoliday es el calendario de festivos de las entidades: todas son colombianas por ahora.
func isColombianHoliday(d domain.Date) bool {
	return slices.Contains(domain.ColombianHolidays(d.Year), d)
}
