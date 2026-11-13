// Package postgres reads the events of infra.outbox for the Events API.
package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/webhooks/internal/app"
)

var _ app.EventReader = Store{}

// Store reads the outbox over a connection or a transaction.
type Store struct{ q postgres.Querier }

// NewStore creates the store over q, the pool or a Tx.
func NewStore(q postgres.Querier) Store { return Store{q: q} }

// The two reads of a page, with and without a type, so each one uses its index (migration 0031). An event shows up
// once its transaction is below the snapshot's xmin: every transaction that could still commit an event is at or
// above it.
const (
	eventColumns = `SELECT id, type, subject, legal_entity_id, correlation_id, schema_version, payload, created_at, created_xact
		FROM infra.outbox`
	// the window, the visibility and the cursor ($2, $3), in order, up to $4; the type, when there is one, is $5
	visibleAfterPosition = `created_at >= now() - interval '30 days' AND created_xact < pg_snapshot_xmin(pg_current_snapshot())
			AND (created_xact, id) > ($2::xid8, $3)
		ORDER BY created_xact, id LIMIT $4`
	eventsOfEntity = eventColumns + ` WHERE legal_entity_id = $1 AND ` + visibleAfterPosition
	eventsOfType   = eventColumns + ` WHERE legal_entity_id = $1 AND type = $5 AND ` + visibleAfterPosition
)

// EventsAfter are the first limit events of the entity of the last 30 days after p (app.EventReader).
func (s Store) EventsAfter(ctx context.Context, entity ids.ID, typ string, p app.Position, limit int) ([]app.Event, error) {
	args := []any{entity, p.Xact, p.ID, limit}
	query := eventsOfEntity
	if typ != "" {
		query, args = eventsOfType, append(args, typ)
	}
	rows, err := s.q.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read the events: %w", err)
	}
	events, err := pgx.CollectRows(rows, scanEvent)
	if err != nil {
		return nil, fmt.Errorf("read the events: %w", err)
	}
	return events, nil
}

func scanEvent(row pgx.CollectableRow) (app.Event, error) {
	var e app.Event
	var correlation *ids.ID
	var payload []byte
	if err := row.Scan(&e.ID, &e.Type, &e.Subject, &e.LegalEntityID, &correlation, &e.SchemaVersion, &payload, &e.Time, &e.Xact); err != nil {
		return app.Event{}, err
	}
	if correlation != nil {
		e.CorrelationID = *correlation
	}
	e.Data = payload
	return e, nil
}
