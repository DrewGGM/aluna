//go:build integration

package webhooks_test

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
	"github.com/DrewGGM/aluna/internal/webhooks"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, &testPool)) }

const (
	opened  = "aluna.deposits.account.opened.v1"
	settled = "aluna.payments.order.settled.v1"
)

// emit emits events of the types for entity, one transaction each, and returns their ids in order.
func emit(t *testing.T, entity ids.ID, types ...string) []ids.ID {
	t.Helper()
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	emitted := make([]ids.ID, len(types))
	for i, typ := range types {
		emitted[i] = ids.New()
		err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
			return events.Emit(t.Context(), tx, outbox.Event{
				ID: emitted[i], Type: typ, Subject: "test/" + emitted[i].String(), LegalEntityID: entity, SchemaVersion: 1,
				Data: map[string]int{"n": i},
			})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return emitted
}

func idsOf(page webhooks.EventPage) []ids.ID {
	out := make([]ids.ID, len(page.Events))
	for i, e := range page.Events {
		out[i] = e.ID
	}
	return out
}

// read is the page once it has want events: an event shows up when every transaction older than its own has ended,
// and other tests' transactions run meanwhile.
func read(t *testing.T, entity ids.ID, typ, cursor string, limit, want int) webhooks.EventPage {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		page, err := webhooks.New().Events(t.Context(), testPool, entity, typ, cursor, limit)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Events) >= want || time.Now().After(deadline) {
			return page
		}
	}
}

// TestTheEventsOfAnEntityPage (FR-46): in order, without repeating one, and only the token's entity's; the last page
// also has a cursor, from which the events emitted later are read.
func TestTheEventsOfAnEntityPage(t *testing.T) {
	entity := ids.New()
	emitted := emit(t, entity, opened, settled, opened)
	emit(t, ids.New(), opened)

	first := read(t, entity, "", "", 2, 2)
	second := read(t, entity, "", first.Next, 2, 1)

	if !slices.Equal(append(idsOf(first), idsOf(second)...), emitted) || !first.HasMore || second.HasMore || second.Next == "" {
		t.Fatalf("pages %v (more %v) and %v (more %v, next %q), want %v", idsOf(first), first.HasMore, idsOf(second), second.HasMore,
			second.Next, emitted)
	}
	later := emit(t, entity, settled)
	next := read(t, entity, "", second.Next, 2, 1)
	if !slices.Equal(idsOf(next), later) || next.HasMore {
		t.Fatalf("after the last page %v, want %v", idsOf(next), later)
	}
	if empty := read(t, entity, "", next.Next, 2, 0); len(empty.Events) != 0 || empty.Next != next.Next {
		t.Fatalf("an empty page %+v, want its cursor back", empty)
	}
}

// TestAnEventOfATransactionStillRunningWaits: an event shows up once every transaction older than its own has ended,
// so one that commits after a younger one is not skipped by a cursor taken meanwhile; and the events go in the order of
// their transactions, so the older one's go first, also the one it emitted after the younger had committed.
func TestAnEventOfATransactionStillRunningWaits(t *testing.T) {
	entity := ids.New()
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	older, later := ids.New(), ids.New()
	emitted, release, commit := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	t.Cleanup(free) // an open transaction would hold back every other test's events
	go func() {
		commit <- postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
			event := outbox.Event{ID: older, Type: opened, Subject: "test", LegalEntityID: entity, SchemaVersion: 1, Data: struct{}{}}
			if err := events.Emit(t.Context(), tx, event); err != nil {
				return err
			}
			close(emitted)
			<-release
			event.ID = later
			return events.Emit(t.Context(), tx, event)
		})
	}()
	select {
	case <-emitted:
	case err := <-commit:
		t.Fatalf("older transaction: %v", err)
	}
	younger := emit(t, entity, settled)

	page, err := webhooks.New().Events(t.Context(), testPool, entity, "", "", 50)
	free()
	if err != nil || len(page.Events) != 0 {
		t.Fatalf("while the older transaction runs: %v, %v; want nothing yet", idsOf(page), err)
	}
	if err := <-commit; err != nil {
		t.Fatal(err)
	}
	if page := read(t, entity, "", "", 50, 3); !slices.Equal(idsOf(page), []ids.ID{older, later, younger[0]}) {
		t.Fatalf("events %v, want the older transaction's first", idsOf(page))
	}
}

// TestTheEventsOfAType: the type filters, and an event comes as a CloudEvent with Aluna's extensions.
func TestTheEventsOfAType(t *testing.T) {
	entity := ids.New()
	emitted := emit(t, entity, opened, settled, opened)

	page := read(t, entity, opened, "", 50, 2)

	if !slices.Equal(idsOf(page), []ids.ID{emitted[0], emitted[2]}) {
		t.Fatalf("events of type %s = %v", opened, idsOf(page))
	}
	raw, err := json.Marshal(page.Events[0])
	if err != nil {
		t.Fatal(err)
	}
	var ce struct {
		SpecVersion, Source, Type, LegalEntityID string
		Data                                     struct{ N int }
	}
	if err := json.Unmarshal(raw, &ce); err != nil || ce.SpecVersion != "1.0" || ce.Source != "aluna/deposits" || ce.Type != opened ||
		ce.LegalEntityID != entity.String() || ce.Data.N != 0 {
		t.Fatalf("event %s", raw)
	}
}

// TestAnEventOlderThan30DaysIsGone: the API keeps 30 days, even before the purge removes them.
func TestAnEventOlderThan30DaysIsGone(t *testing.T) {
	entity := ids.New()
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO infra.outbox (id, type, subject, legal_entity_id, schema_version, payload, created_at)
			VALUES ($1, $2, 'test', $3, 1, '{}', now() - interval '31 days')`, ids.New(), opened, entity)
		return err
	})
	recent := emit(t, entity, opened)

	if page := read(t, entity, "", "", 50, 1); !slices.Equal(idsOf(page), recent) {
		t.Fatalf("events %v, want only %v", idsOf(page), recent)
	}
}

func TestAWrongPageIsInvalid(t *testing.T) {
	entity := ids.New()
	for name, c := range map[string]struct {
		cursor string
		limit  int
	}{
		"limit 0":             {"", 0},
		"limit 201":           {"", 201},
		"a cursor of nowhere": {"nowhere", 10},
		"a cursor too short":  {"AQ", 10},
	} {
		if _, err := webhooks.New().Events(t.Context(), testPool, entity, "", c.cursor, c.limit); !errors.Is(err, webhooks.ErrInvalidPage) {
			t.Errorf("%s: %v, want ErrInvalidPage", name, err)
		}
	}
}
