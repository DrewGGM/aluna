// Package webhooks is the facade of what Aluna tells integrators about its events (FR-46): the Events API, the events
// of the last 30 days of an entity, as CloudEvents.
package webhooks

import (
	"context"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
	pg "github.com/DrewGGM/aluna/internal/webhooks/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/webhooks/internal/app"
)

// EventPage is a page of an entity's events, in the order of the transactions that emitted them; Next is where the
// following one starts, also on the last page, and HasMore whether there are events after it already.
type EventPage = app.EventPage

// ErrInvalidPage means a page of no events or of more than app.MaxPageSize (200), or a cursor that is not one.
var ErrInvalidPage = app.ErrInvalidPage

// Service is the facade. It holds no state: each operation receives the connection.
type Service struct{}

// New creates the facade.
func New() *Service { return &Service{} }

// Events is the page of limit events of the entity of the last 30 days that follows cursor, or the first one when it
// is empty, only of type typ unless it is empty (FR-46). q can be the pool or a Tx.
func (s *Service) Events(ctx context.Context, q postgres.Querier, entity ids.ID, typ, cursor string, limit int) (EventPage, error) {
	return app.Events(ctx, pg.NewStore(q), entity, typ, cursor, limit)
}
