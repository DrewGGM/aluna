//go:build integration

package main

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/ids"
)

// TestEntityCreateOverTheCLI: aluna entity create registers, as the tables' owner, an entity on its business date with
// its functional currency and prints its ID, a new one or the one --id gives.
func TestEntityCreateOverTheCLI(t *testing.T) {
	name := "Entity " + ids.New().String()
	out, errOut, code := createEntity(t, "--name", name, "--business-date", "2026-10-30", "--currency", "COP")
	created, err := ids.Parse(strings.TrimSpace(out))
	if code != 0 || err != nil {
		t.Fatalf("entity create = %d, stdout %q, stderr %q: want 0 and the entity's ID", code, out, errOut)
	}
	given := ids.New()
	if out, errOut, code := createEntity(t, "--id", given.String(), "--name", name, "--business-date", "2026-11-03", "--currency", "COP"); code != 0 ||
		strings.TrimSpace(out) != given.String() {
		t.Fatalf("entity create --id = %d, stdout %q, stderr %q; want %s", code, out, errOut, given)
	}

	want := map[string]bool{created.String() + " 2026-10-30 COP": true, given.String() + " 2026-11-03 COP": true}
	if got := entityRows(t, name); len(got) != len(want) || !want[got[0]] || !want[got[1]] {
		t.Fatalf("entities named %q: %v, want only %v", name, got, want)
	}
}

// TestEntityCreateRefusesAnInvalidEntity: a Saturday, a business day that has not started yet or an unknown currency
// exits with 1 and registers nothing.
func TestEntityCreateRefusesAnInvalidEntity(t *testing.T) {
	name := "Entity " + ids.New().String()
	invalid := []struct{ what, day, currency string }{
		{"a Saturday", "2026-10-31", "COP"},
		{"a business day to come", "2099-01-02", "COP"},
		{"an unknown currency", "2026-10-30", "XXX"},
	}
	for _, tc := range invalid {
		out, errOut, code := createEntity(t, "--name", name, "--business-date", tc.day, "--currency", tc.currency)
		if code != 1 || out != "" || !strings.Contains(errOut, "invalid legal entity") {
			t.Errorf("an entity on %s = %d, stdout %q, stderr %q", tc.what, code, out, errOut)
		}
	}
	if got := entityRows(t, name); len(got) != 0 {
		t.Fatalf("entities named %q: %v, want none", name, got)
	}
}

// createEntity runs aluna entity create with args on the test database as its owner, and returns what it printed and
// its exit code.
func createEntity(t *testing.T, args ...string) (stdout, errOut string, code int) {
	t.Helper()
	t.Setenv("ALUNA_DATABASE_URL", testDSN)
	defer func(w io.Writer) { stderr = w }(stderr)
	var out, errs bytes.Buffer
	stderr = &errs
	code = run(append([]string{"entity", "create"}, args...), &out)
	return out.String(), errs.String(), code
}

// entityRows lists, as the owner, the entities named name with their business date and currency.
func entityRows(t *testing.T, name string) []string {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context()) //nolint:errcheck // the test is done with the connection
	rows, err := conn.Query(t.Context(), `SELECT id::text || ' ' || business_date::text || ' ' || functional_currency
		FROM calendar.legal_entities WHERE name = $1`, name)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return got
}
