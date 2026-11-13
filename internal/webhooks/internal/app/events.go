// Package app has the use cases of the Events API (FR-46): the pages of an entity's events.
package app

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/outbox"
)

// MaxPageSize is the most events a page has.
const MaxPageSize = 200

// ErrInvalidPage means a page of no events or of more than MaxPageSize, or a cursor that is not one.
var ErrInvalidPage = errors.New("invalid page")

var errUnknownCursor = fmt.Errorf("%w: unknown cursor", ErrInvalidPage)

// Position is a position among an entity's events: the events after it are those of a later transaction (Xact, its
// xid8), or of the same one with an ID above ID. The zero Position is before every event.
type Position struct {
	Xact uint64
	ID   ids.ID
}

// Event is an event with the transaction that emitted it.
type Event struct {
	outbox.StoredEvent
	Xact uint64
}

// EventPage is a page of an entity's events, in the order of the transactions that emitted them. Next is where the
// following page starts, also on the last page, so a consumer keeps polling from it; HasMore says whether there are
// events after this page already.
type EventPage struct {
	Events  []outbox.StoredEvent
	Next    string
	HasMore bool
}

// EventReader reads the outbox.
type EventReader interface {
	// EventsAfter are the first limit events of the entity of the last 30 days after p, in (transaction, id) order,
	// only of type typ unless it is empty, and only of transactions older than every one still running.
	EventsAfter(ctx context.Context, entity ids.ID, typ string, p Position, limit int) ([]Event, error)
}

// Events is the page of limit events of the entity, of type typ unless it is empty, that follows cursor, or the first
// one of the last 30 days when cursor is empty (FR-46). Paging never repeats an event nor skips one: an event shows up
// once every transaction older than its own has ended, and every event after a cursor is of a later one.
func Events(ctx context.Context, r EventReader, entity ids.ID, typ, cursor string, limit int) (EventPage, error) {
	if limit < 1 || limit > MaxPageSize {
		return EventPage{}, fmt.Errorf("%w: a page has 1 to %d events, not %d", ErrInvalidPage, MaxPageSize, limit)
	}
	p, err := positionOf(cursor)
	if err != nil {
		return EventPage{}, err
	}
	events, err := r.EventsAfter(ctx, entity, typ, p, limit+1)
	if err != nil {
		return EventPage{}, err
	}
	page := EventPage{Next: cursor, HasMore: len(events) > limit}
	if page.HasMore {
		events = events[:limit]
	}
	page.Events = make([]outbox.StoredEvent, len(events))
	for i, e := range events {
		page.Events[i] = e.StoredEvent
	}
	if len(events) > 0 {
		last := events[len(events)-1]
		page.Next = cursorOf(Position{Xact: last.Xact, ID: last.ID})
	}
	return page, nil
}

// cursorOf is p, opaque: base64 of its transaction, as a uvarint, and its ID.
func cursorOf(p Position) string {
	raw := append(binary.AppendUvarint(nil, p.Xact), p.ID[:]...)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// positionOf is the position cursor marks, or the zero Position if it is empty: any transaction and ID decode to a
// position among the entity's own events, so all a cursor needs is to be one.
func positionOf(cursor string) (Position, error) {
	if cursor == "" {
		return Position{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return Position{}, errUnknownCursor
	}
	xact, n := binary.Uvarint(raw)
	if n <= 0 || len(raw)-n != len(ids.ID{}) {
		return Position{}, errUnknownCursor
	}
	return Position{Xact: xact, ID: ids.ID(raw[n:])}, nil
}
