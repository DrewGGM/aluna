//go:build integration || e2e || load || restore

package pgtest

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DrewGGM/aluna/internal/ids"
)

// Event is an event stored in infra.outbox, with its payload decoded; its numbers are json.Number, so that
// amounts are compared as the JSON text, without going through float64.
type Event struct {
	Type          string
	LegalEntityID ids.ID
	SchemaVersion int
	Data          map[string]any
}

// EventsAbout are the committed events about subject, in the order they were emitted. An error stops the test.
func EventsAbout(t testing.TB, pool *pgxpool.Pool, subject string) []Event {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT type, legal_entity_id, schema_version, payload FROM infra.outbox
		WHERE subject = $1 ORDER BY created_at, id`, subject)
	if err != nil {
		t.Fatal(err)
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var e Event
		var payload []byte
		if err := row.Scan(&e.Type, &e.LegalEntityID, &e.SchemaVersion, &payload); err != nil {
			return Event{}, err
		}
		dec := json.NewDecoder(bytes.NewReader(payload))
		dec.UseNumber()
		return e, dec.Decode(&e.Data)
	})
	if err != nil {
		t.Fatal(err)
	}
	return events
}
