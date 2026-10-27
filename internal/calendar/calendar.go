// Package calendar es el módulo de calendario (ADR-0006): entidades legales, su fecha de negocio y los
// días hábiles.
package calendar

import (
	"context"

	pg "github.com/DrewGGM/aluna/internal/calendar/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/calendar/internal/app"
	"github.com/DrewGGM/aluna/internal/calendar/internal/domain"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// Date es un día calendario, sin hora ni zona.
type Date = domain.Date

// PeriodStatus es el estado de un periodo contable.
type PeriodStatus = app.PeriodStatus

// Estados de un periodo contable.
const (
	PeriodOpen    = app.PeriodOpen
	PeriodClosing = app.PeriodClosing
	PeriodClosed  = app.PeriodClosed
)

// Errores del calendario.
var (
	ErrLegalEntityNotFound = app.ErrLegalEntityNotFound
	ErrStaleBusinessDate   = app.ErrStaleBusinessDate
	ErrPeriodClosed        = app.ErrPeriodClosed
)

// Service es la fachada del calendario. No guarda estado: cada operación recibe la conexión o la
// transacción en la que corre.
type Service struct{}

// New crea el servicio del calendario.
func New() *Service { return &Service{} }

// BusinessDate es la fecha de negocio de la entidad legal. Un caso de uso la lee al empezar su
// transacción y la usa en todo lo que asienta: la lectura no bloquea el rollover. q puede ser el pool o
// una Tx.
func (s *Service) BusinessDate(ctx context.Context, q postgres.Querier, legalEntity ids.ID) (Date, error) {
	return pg.NewStore(q).BusinessDate(ctx, legalEntity)
}

// Rollover avanza dentro de tx la fecha de negocio de la entidad de expected al día hábil siguiente y
// devuelve la nueva. Si la fecha ya no es expected (otro rollover la avanzó), es ErrStaleBusinessDate y
// no cambia nada.
func (s *Service) Rollover(ctx context.Context, tx postgres.Tx, legalEntity ids.ID, expected Date) (Date, error) {
	return app.Rollover(ctx, pg.NewStore(tx), legalEntity, expected)
}

// PeriodStatus es el estado del periodo contable de d para la entidad (FR-15). Un mes que el cierre no
// tocó está abierto. q puede ser el pool o una Tx.
func (s *Service) PeriodStatus(ctx context.Context, q postgres.Querier, legalEntity ids.ID, d Date) (PeriodStatus, error) {
	return app.PeriodStatusOf(ctx, pg.NewStore(q), legalEntity, d)
}

// EnsureOpen es ErrPeriodClosed si d cae en un periodo cerrado de la entidad: el ledger lo pide antes de
// asentar. Lee sin bloquear, como BusinessDate.
func (s *Service) EnsureOpen(ctx context.Context, q postgres.Querier, legalEntity ids.ID, d Date) error {
	return app.EnsureOpen(ctx, pg.NewStore(q), legalEntity, d)
}
