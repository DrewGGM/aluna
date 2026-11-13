//go:build integration

package httpapi

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// entityOn registers, as the tables' owner at now, an entity whose business date is day, and returns it with the outbox
// the servers emit to.
func entityOn(t *testing.T, day calendar.Date, now time.Time) (ids.ID, *outbox.Outbox) {
	t.Helper()
	entity := ids.New()
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		return calendar.CreateLegalEntity(t.Context(), tx, calendar.LegalEntity{ID: entity, Name: "Entity", BusinessDate: day, Currency: "COP"}, now)
	})
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	return entity, events
}

var october30 = calendar.Date{Year: 2026, Month: time.October, Day: 30}

// TestTheBusinessDateOverHTTP: an operator reads the token entity's business date; an integrator cannot, and a token
// whose entity is not registered is 404 legal_entity_not_found.
func TestTheBusinessDateOverHTTP(t *testing.T) {
	entity, events := entityOn(t, october30, time.Now())

	var got struct {
		LegalEntityID string `json:"legal_entity_id"`
		BusinessDate  string `json:"business_date"`
	}
	if status := getJSON(t, apiServer(t, entity), "/v1/business-date", &got); status != http.StatusOK ||
		got.LegalEntityID != entity.String() || got.BusinessDate != "2026-10-30" {
		t.Fatalf("business date = %d %+v, want 200 with 2026-10-30", status, got)
	}
	var problem Problem
	integrator := apiServerAsRole(t, entity, events, "integrator-a", "integrator")
	if status := getJSON(t, integrator, "/v1/business-date", &problem); status != http.StatusForbidden {
		t.Fatalf("an integrator reading the business date = %d", status)
	}
	if status := getJSON(t, apiServer(t, ids.New()), "/v1/business-date", &problem); status != http.StatusNotFound || problem.Code != "legal_entity_not_found" {
		t.Fatalf("an entity that is not registered = %d %+v, want 404 legal_entity_not_found", status, problem)
	}
}

// TestARolloverOverHTTP (FR-13): the system closes Friday 2026-10-30 and the entity moves to Tuesday 2026-11-03
// (Monday the 2nd is a holiday), with its event and its audit record; a retry with the same Idempotency-Key gets the
// same response without advancing again, another key with the same date is 409 business_date_stale, and an operator
// cannot roll over. A body without the date is 400 and is not stored under its key.
func TestARolloverOverHTTP(t *testing.T) {
	entity, events := entityOn(t, october30, time.Now())
	system := apiServerAsRole(t, entity, events, "scheduler", "system")
	key, body := ids.New().String(), `{"business_date":"2026-10-30"}`
	expectProblem(t, "a rollover without a date", postTo(t, system, "/v1/business-date/rollover", key, `{}`), http.StatusBadRequest, "invalid_request")

	rolled, correlation := auditedRequest(t, system, http.MethodPost, "/v1/business-date/rollover", key, body)

	want := `{"business_date":"2026-11-03","closed_business_date":"2026-10-30"}` + "\n"
	if rolled.status != http.StatusOK || string(rolled.body) != want {
		t.Fatalf("rollover = %d %s, want 200 %s", rolled.status, rolled.body, want)
	}
	if r := onlyRecord(t, correlation); r.Actor != "scheduler" || r.Command != "POST /v1/business-date/rollover" || r.Result != resultOK ||
		r.After["closed_business_date"] != "2026-10-30" || r.After["business_date"] != "2026-11-03" {
		t.Fatalf("audit record %+v", r.Record)
	}
	if replay := postTo(t, system, "/v1/business-date/rollover", key, body); replay.status != rolled.status || !bytes.Equal(replay.body, rolled.body) {
		t.Fatalf("replay = %d %s, want the stored response", replay.status, replay.body)
	}
	expectProblem(t, "another rollover from 2026-10-30", postTo(t, system, "/v1/business-date/rollover", ids.New().String(), body),
		http.StatusConflict, "business_date_stale")
	expectProblem(t, "an operator rolling over", postTo(t, apiServer(t, entity), "/v1/business-date/rollover", ids.New().String(), body),
		http.StatusForbidden, "forbidden")
	expectRolledOnce(t, entity)
}

// expectRolledOnce checks that entity moved from 2026-10-30 to 2026-11-03 once, with one event.
func expectRolledOnce(t *testing.T, entity ids.ID) {
	t.Helper()
	if got := textOf(t, `SELECT business_date::text FROM calendar.legal_entities WHERE id = $1`, entity); got != "2026-11-03" {
		t.Fatalf("business date = %s, want 2026-11-03: it advanced more than once", got)
	}
	if got := textOf(t, `SELECT count(*)::text || ' ' || min(payload::text) FROM infra.outbox WHERE legal_entity_id = $1
		AND type = 'aluna.calendar.business-date.rolled.v1'`, entity); got != `1 {"business_date": "2026-11-03", "closed_business_date": "2026-10-30"}` {
		t.Fatalf("rolled events = %s, want one from 2026-10-30 to 2026-11-03", got)
	}
}

// TestTheBusinessDateDoesNotRunAheadOverHTTP: closing a business date that has not started yet is 422
// business_date_ahead, and the date stays. The entity is registered on 2099-01-02 itself, so by today's clock its
// business date is ahead, as one rolled over on a Friday evening is on Saturday.
func TestTheBusinessDateDoesNotRunAheadOverHTTP(t *testing.T) {
	future := calendar.Date{Year: 2099, Month: time.January, Day: 2}
	entity, events := entityOn(t, future, time.Date(2099, time.January, 2, 15, 0, 0, 0, time.UTC))
	system := apiServerAsRole(t, entity, events, "scheduler", "system")

	expectProblem(t, "a rollover of 2099-01-02", postTo(t, system, "/v1/business-date/rollover", ids.New().String(), `{"business_date":"2099-01-02"}`),
		http.StatusUnprocessableEntity, "business_date_ahead")
	if got := textOf(t, `SELECT business_date::text FROM calendar.legal_entities WHERE id = $1`, entity); got != future.String() {
		t.Fatalf("business date = %s, want %s", got, future)
	}
}
