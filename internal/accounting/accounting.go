// Package accounting is the facade of the mapping of the internal accounts to CUIF codes (FR-36, ADR-0011), of the
// daily GL projected from the journal by them, and of the close of a period (FR-37): the journal keeps its internal
// codes, and what they are in the regulator's catalog changes by period without touching it.
package accounting

import (
	"context"
	"io"

	pg "github.com/DrewGGM/aluna/internal/accounting/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/accounting/internal/app"
	"github.com/DrewGGM/aluna/internal/accounting/internal/domain"
	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// Mapping says which CUIF code the internal accounts of a pattern are in a period.
type Mapping = domain.Mapping

// Errors of the mappings, the GL and the close of a period.
var (
	ErrInvalidMapping  = domain.ErrInvalidMapping
	ErrMappingOverlap  = domain.ErrMappingOverlap
	ErrNoMapping       = domain.ErrNoMapping
	ErrNotProjected    = domain.ErrNotProjected
	ErrDayUnmapped     = domain.ErrDayUnmapped
	ErrPeriodNotSquare = domain.ErrPeriodNotSquare
	ErrOpenBreaks      = domain.ErrOpenBreaks
)

// ParseMappings reads a CSV file of mappings, with the header pattern,cuif_code,valid_from,valid_to:
// ErrInvalidMapping if a row is not valid, and ErrMappingOverlap if two of them overlap.
func ParseMappings(file io.Reader) ([]Mapping, error) { return domain.ParseMappings(file) }

// Events publishes accounting events; outbox.Outbox implements it.
type Events interface {
	Emit(ctx context.Context, tx postgres.Tx, e outbox.Event) error
}

// Service keeps the mappings and closes periods.
type Service struct {
	calendar *calendar.Service
	events   Events
}

// New creates the service.
func New(events Events) *Service { return &Service{calendar: calendar.New(), events: events} }

// mappings is app.Mappings over q.
type mappings struct {
	pg.Store
	q        postgres.Querier
	calendar *calendar.Service
}

func (s *Service) over(q postgres.Querier) mappings {
	return mappings{Store: pg.NewStore(q), q: q, calendar: s.calendar}
}

func (m mappings) BusinessDate(ctx context.Context, entity ids.ID) (calendar.Date, error) {
	return m.calendar.BusinessDate(ctx, m.q, entity)
}

// CheckMappings reports whether the entity can load mappings: ErrInvalidMapping if one starts before its business
// date, since it would change the code of closed days, and ErrMappingOverlap if one overlaps a stored one. q can be
// the pool or a Tx.
func (s *Service) CheckMappings(ctx context.Context, q postgres.Querier, entity ids.ID, m []Mapping) error {
	return app.CheckMappings(ctx, s.over(q), entity, m)
}

// LoadMappings stores mappings within tx: ErrInvalidMapping if one starts before the business date, or
// ErrMappingOverlap. A mapping is never changed.
func (s *Service) LoadMappings(ctx context.Context, tx postgres.Tx, entity ids.ID, m []Mapping) error {
	return app.LoadMappings(ctx, s.over(tx), entity, m)
}

// MapToCUIF is the CUIF code of the entity's internal account with code account on day: that of the most specific
// mapping in force, or ErrNoMapping. q can be the pool or a Tx.
func (s *Service) MapToCUIF(ctx context.Context, q postgres.Querier, entity ids.ID, account string, day calendar.Date) (string, error) {
	return app.MapToCUIF(ctx, s.over(q), entity, account, day)
}
