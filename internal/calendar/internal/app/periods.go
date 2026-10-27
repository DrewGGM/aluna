package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/DrewGGM/aluna/internal/calendar/internal/domain"
	"github.com/DrewGGM/aluna/internal/ids"
)

// PeriodStatus es el estado de un periodo contable (FR-15).
type PeriodStatus string

// Estados de un periodo contable: open → closing → closed.
const (
	PeriodOpen    PeriodStatus = "open"
	PeriodClosing PeriodStatus = "closing"
	PeriodClosed  PeriodStatus = "closed"
)

// ErrPeriodClosed indica una fecha de negocio dentro de un periodo cerrado: no se asienta.
var ErrPeriodClosed = errors.New("periodo contable cerrado")

// PeriodStore lee el estado de los periodos.
type PeriodStore interface {
	// PeriodStatus es el estado del periodo yyyy-mm de la entidad; found es false si no tiene fila.
	PeriodStatus(ctx context.Context, entity ids.ID, period string) (status PeriodStatus, found bool, err error)
}

// PeriodStatusOf es el estado del periodo de d. Un mes sin fila está abierto: el cierre crea la fila.
func PeriodStatusOf(ctx context.Context, store PeriodStore, entity ids.ID, d domain.Date) (PeriodStatus, error) {
	status, found, err := store.PeriodStatus(ctx, entity, periodOf(d))
	if err != nil {
		return "", err
	}
	if !found {
		return PeriodOpen, nil
	}
	return status, nil
}

// EnsureOpen es ErrPeriodClosed si d cae en un periodo cerrado. Un periodo en cierre todavía admite
// asientos (los ajustes del cierre).
func EnsureOpen(ctx context.Context, store PeriodStore, entity ids.ID, d domain.Date) error {
	status, err := PeriodStatusOf(ctx, store, entity, d)
	if err != nil {
		return err
	}
	if status == PeriodClosed {
		return fmt.Errorf("%w: %s", ErrPeriodClosed, periodOf(d))
	}
	return nil
}

func periodOf(d domain.Date) string { return fmt.Sprintf("%04d-%02d", d.Year, d.Month) }
