//go:build integration

package accounting_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DrewGGM/aluna/internal/accounting"
	pg "github.com/DrewGGM/aluna/internal/accounting/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, &testPool)) }

const header = "pattern,cuif_code,valid_from,valid_to\n"

func mappings(t *testing.T, rows string) []accounting.Mapping {
	t.Helper()
	parsed, err := accounting.ParseMappings(strings.NewReader(header + rows))
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// newEntity is an entity whose business date is 2026-01-01, so the mappings of these tests start on time.
func newEntity(t *testing.T) ids.ID {
	t.Helper()
	entity := ids.New()
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO calendar.legal_entities (id, name, business_date, functional_currency)
			VALUES ($1, 'Entity', '2026-01-01', 'COP')`, entity)
		return err
	})
	return entity
}

func load(t *testing.T, s *accounting.Service, entity ids.ID, m []accounting.Mapping) error {
	t.Helper()
	return postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error { return s.LoadMappings(t.Context(), tx, entity, m) })
}

func on(t *testing.T, s string) calendar.Date {
	t.Helper()
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		t.Fatal(err)
	}
	return calendar.Date{Year: d.Year(), Month: d.Month(), Day: d.Day()}
}

// TestAnAccountMapsByItsMostSpecificPatternInForce (FR-36): a period ends the day before the next, and another entity
// has its own mappings.
func TestAnAccountMapsByItsMostSpecificPatternInForce(t *testing.T) {
	s, entity, other := accounting.New(), newEntity(t), ids.New()
	err := load(t, s, entity, mappings(t, "L.,210000,2026-01-01,2030-01-01\nL.TAX.GMF,244500,2026-01-01,2028-01-01\nL.TAX.GMF,2445000001,2028-01-01,2030-01-01\n"))
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ account, day, want string }{
		{"L.TAX.GMF", "2027-12-31", "244500"},
		{"L.TAX.GMF", "2028-01-01", "2445000001"},
		{"L.OTHER", "2029-12-31", "210000"},
	} {
		if got, err := s.MapToCUIF(t.Context(), testPool, entity, c.account, on(t, c.day)); err != nil || got != c.want {
			t.Errorf("%s on %s = %s, %v, want %s", c.account, c.day, got, err, c.want)
		}
	}
	if got, err := s.MapToCUIF(t.Context(), testPool, entity, "L.OTHER", on(t, "2030-01-01")); !errors.Is(err, accounting.ErrNoMapping) {
		t.Errorf("after the end = %s, %v, want ErrNoMapping", got, err)
	}
	if got, err := s.MapToCUIF(t.Context(), testPool, other, "L.OTHER", on(t, "2026-03-02")); !errors.Is(err, accounting.ErrNoMapping) {
		t.Errorf("for another entity = %s, %v, want ErrNoMapping", got, err)
	}
}

// TestMappingsOfAPatternDoNotOverlap: checking says so before the load, and the load itself refuses it, leaving none
// of its file; a period that only touches is fine.
func TestMappingsOfAPatternDoNotOverlap(t *testing.T) {
	s, entity := accounting.New(), newEntity(t)
	if err := load(t, s, entity, mappings(t, "L.,210000,2026-01-01,2027-01-01\n")); err != nil {
		t.Fatal(err)
	}

	overlapping := mappings(t, "A.,110000,2026-01-01,2027-01-01\nL.,210001,2026-06-01,2028-01-01\n")
	if err := s.CheckMappings(t.Context(), testPool, entity, overlapping); !errors.Is(err, accounting.ErrMappingOverlap) {
		t.Fatalf("CheckMappings = %v, want ErrMappingOverlap", err)
	}
	if err := load(t, s, entity, overlapping); !errors.Is(err, accounting.ErrMappingOverlap) {
		t.Fatalf("LoadMappings = %v, want ErrMappingOverlap", err)
	}
	if got, err := s.MapToCUIF(t.Context(), testPool, entity, "A.CASH", on(t, "2026-03-02")); !errors.Is(err, accounting.ErrNoMapping) {
		t.Fatalf("A.CASH = %s, %v: the failed load left a row", got, err)
	}

	next := mappings(t, "L.,210001,2027-01-01,2028-01-01\n")
	if err := s.CheckMappings(t.Context(), testPool, entity, next); err != nil {
		t.Fatal(err)
	}
	if err := load(t, s, entity, next); err != nil {
		t.Fatal(err)
	}
	if err := load(t, s, newEntity(t), overlapping); err != nil {
		t.Errorf("another entity's period: %v", err)
	}
}

// TestAnotherPatternDoesNotOverlap: a period is taken by pattern.
func TestAnotherPatternDoesNotOverlap(t *testing.T) {
	s, entity := accounting.New(), newEntity(t)
	if err := load(t, s, entity, mappings(t, "L.,210000,2026-01-01,2027-01-01\n")); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckMappings(t.Context(), testPool, entity, mappings(t, "A.,110000,2026-01-01,2027-01-01\n")); err != nil {
		t.Fatal(err)
	}
}

// TestTheDatabaseRefusesAnOverlapToo: what the check refuses, the constraints refuse too, for a load that got past it
// while another committed: with the same start (the primary key) or inside the period (the exclusion).
func TestTheDatabaseRefusesAnOverlapToo(t *testing.T) {
	for name, second := range map[string]string{"the same start": "L.,210000,2026-01-01,2027-01-01\n", "inside it": "L.,210000,2026-06-01,2027-01-01\n"} {
		entity := ids.New()
		first := mappings(t, "L.,210000,2026-01-01,2027-01-01\n")[0]
		err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
			if err := pg.NewStore(tx).InsertMapping(t.Context(), entity, first); err != nil {
				return err
			}
			return pg.NewStore(tx).InsertMapping(t.Context(), entity, mappings(t, second)[0])
		})
		if !errors.Is(err, accounting.ErrMappingOverlap) {
			t.Errorf("%s: second insert = %v, want ErrMappingOverlap", name, err)
		}
	}
}

// TestAMappingCannotStartInThePast: checking and loading both refuse it, by the entity's business date.
func TestAMappingCannotStartInThePast(t *testing.T) {
	s, entity := accounting.New(), newEntity(t)
	past := mappings(t, "L.,210000,2025-12-31,2027-01-01\n")
	if err := s.CheckMappings(t.Context(), testPool, entity, past); !errors.Is(err, accounting.ErrInvalidMapping) {
		t.Errorf("CheckMappings = %v, want ErrInvalidMapping", err)
	}
	if err := load(t, s, entity, past); !errors.Is(err, accounting.ErrInvalidMapping) {
		t.Errorf("LoadMappings = %v, want ErrInvalidMapping", err)
	}
	if err := load(t, s, ids.New(), past); err == nil {
		t.Error("an entity without a business date loaded it")
	}
}

// TestAMappingDoesNotChange: not by the application's role, and not by the owner.
func TestAMappingDoesNotChange(t *testing.T) {
	s, entity := accounting.New(), newEntity(t)
	if err := load(t, s, entity, mappings(t, "L.,210000,2026-01-01,2027-01-01\n")); err != nil {
		t.Fatal(err)
	}
	const (
		change = `UPDATE accounting.cuif_mappings SET cuif_code = '210001' WHERE legal_entity_id = $1`
		drop   = `DELETE FROM accounting.cuif_mappings WHERE legal_entity_id = $1`
		wipe   = `TRUNCATE accounting.cuif_mappings`
	)
	for _, statement := range []string{change, drop} {
		if _, err := testPool.Exec(t.Context(), statement, entity); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s: %v, want permission denied", statement, err)
		}
	}
	for _, statement := range []string{change, drop, wipe} {
		pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(t.Context(), `SAVEPOINT guarded`); err != nil {
				return err
			}
			args := []any{entity}
			if statement == wipe {
				args = nil
			}
			_, err := tx.Exec(t.Context(), statement, args...)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "2F003" {
				t.Errorf("%s as the owner: %v, want 2F003", statement, err)
			}
			_, err = tx.Exec(t.Context(), `ROLLBACK TO SAVEPOINT guarded`)
			return err
		})
	}
}

// TestTheExampleFileLoads: what config ships is a valid file.
func TestTheExampleFileLoads(t *testing.T) {
	file, err := os.Open(filepath.Join("..", "..", "config", "cuif-mappings.example.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close() //nolint:errcheck // only read
	parsed, err := accounting.ParseMappings(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := load(t, accounting.New(), newEntity(t), parsed); err != nil {
		t.Fatal(err)
	}
}
