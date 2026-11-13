//go:build integration

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// emitEvent emits an event of type typ for entity.
func emitEvent(t *testing.T, entity ids.ID, typ string) {
	t.Helper()
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	if err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		return events.Emit(t.Context(), tx, outbox.Event{Type: typ, Subject: "test", LegalEntityID: entity, SchemaVersion: 1, Data: struct{}{}})
	}); err != nil {
		t.Fatal(err)
	}
}

// receivedEvent is what this test reads of an event.
type receivedEvent struct {
	SpecVersion   string `json:"specversion"`
	Type          string `json:"type"`
	LegalEntityID string `json:"legalentityid"`
}

// eventPage is what these tests read of a page of events.
type eventPage struct {
	Events     []receivedEvent `json:"events"`
	NextCursor string          `json:"next_cursor"`
	HasMore    bool            `json:"has_more"`
}

// polledEvents reads path until it has events: an event shows up once every transaction older than its own has
// ended, and other tests' transactions run meanwhile.
func polledEvents(t *testing.T, ts *httptest.Server, path string) eventPage {
	t.Helper()
	var page eventPage
	for deadline := time.Now().Add(10 * time.Second); len(page.Events) == 0 && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if status := call(t, ts, http.MethodGet, path, "", &page); status != http.StatusOK {
			t.Fatalf("events = %d", status)
		}
	}
	return page
}

// TestTheEventsOverHTTP (FR-46): the entity's events come to an integrator as CloudEvents, filtered by type, and a
// wrong page is a problem; another entity's are not there.
func TestTheEventsOverHTTP(t *testing.T) {
	entity := ids.New()
	emitEvent(t, entity, "aluna.deposits.account.opened.v1")
	emitEvent(t, entity, "aluna.payments.order.settled.v1")
	emitEvent(t, ids.New(), "aluna.payments.order.settled.v1")
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	ts := apiServerAsRole(t, entity, events, "integrator-a", "integrator")

	page := polledEvents(t, ts, "/v1/events?type=aluna.payments.order.settled.v1")

	want := receivedEvent{SpecVersion: "1.0", Type: "aluna.payments.order.settled.v1", LegalEntityID: entity.String()}
	if len(page.Events) != 1 || page.Events[0] != want || page.NextCursor == "" || page.HasMore {
		t.Fatalf("page %+v, want only %+v", page, want)
	}
	var problem Problem
	if status := call(t, ts, http.MethodGet, "/v1/events?limit=201", "", &problem); status != http.StatusBadRequest || problem.Code != "invalid_request" {
		t.Fatalf("limit 201 = %d %+v", status, problem)
	}
}

// TestAnOperatorDoesNotReadTheEvents: they carry screening and reconciliation results.
func TestAnOperatorDoesNotReadTheEvents(t *testing.T) {
	var problem Problem
	if status := call(t, apiServer(t, ids.New()), http.MethodGet, "/v1/events", "", &problem); status != http.StatusForbidden {
		t.Fatalf("an operator reading the events = %d", status)
	}
}

// TestAWebhookEndpointIsRegisteredOverHTTP (FR-46): an integrator registers a public https URL and gets its secret; a
// private one is 422 invalid_endpoint, and an operator cannot register one.
func TestAWebhookEndpointIsRegisteredOverHTTP(t *testing.T) {
	entity := ids.New()
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO calendar.legal_entities (id, name, business_date, functional_currency)
			VALUES ($1, 'Entity', '2026-03-02', 'COP')`, entity)
		return err
	})
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	ts := apiServerAsRole(t, entity, events, "integrator-a", "integrator")

	var registered struct {
		ID     string `json:"id"`
		URL    string `json:"url"`
		Secret string `json:"secret"`
	}
	if status := call(t, ts, http.MethodPost, "/v1/webhook-endpoints", `{"url":"https://93.184.216.34/hooks"}`, &registered); status != http.StatusCreated {
		t.Fatalf("register = %d", status)
	}
	if registered.ID == "" || registered.URL != "https://93.184.216.34/hooks" || !strings.HasPrefix(registered.Secret, "whsec_") {
		t.Fatalf("registered %+v", registered)
	}
	var problem Problem
	if status := call(t, ts, http.MethodPost, "/v1/webhook-endpoints", `{"url":"https://169.254.169.254/latest/meta-data/"}`, &problem); status != http.StatusUnprocessableEntity ||
		problem.Code != "invalid_endpoint" {
		t.Fatalf("the metadata service = %d %+v", status, problem)
	}
	if status := call(t, apiServer(t, entity), http.MethodPost, "/v1/webhook-endpoints", `{"url":"https://93.184.216.34/hooks"}`, &problem); status != http.StatusForbidden {
		t.Fatalf("an operator registering = %d", status)
	}
}
