//go:build integration

package audit_test

import (
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DrewGGM/aluna/internal/audit"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, &testPool)) }

func record() audit.Record {
	return audit.Record{
		Actor: "operator", Role: "treasurer", Channel: "api", IP: "10.0.0.7", CorrelationID: ids.New().String(),
		Command: "POST /v1/transactions", Targets: []string{ids.New().String()}, Result: "ok",
		Before: map[string]any{"status": "pending"}, After: map[string]any{"status": "posted", "entries": []any{map[string]any{"amount": 100}}},
	}
}

func save(t *testing.T, r audit.Record) error {
	t.Helper()
	return postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error { return audit.New().Record(t.Context(), tx, r) })
}

// stored reads the correlation's record as the owner (aluna_app can only insert).
func stored(t *testing.T, correlation string) (audit.Record, bool) {
	t.Helper()
	var r audit.Record
	found := false
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		err := tx.QueryRow(t.Context(), `SELECT actor, role, channel, ip, correlation_id, command, targets, result, before, after
			FROM audit.records WHERE correlation_id = $1`, correlation).
			Scan(&r.Actor, &r.Role, &r.Channel, &r.IP, &r.CorrelationID, &r.Command, &r.Targets, &r.Result, &r.Before, &r.After)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	return r, found
}

// TestARecordIsStored: Record stores the full record in the transaction it receives.
func TestARecordIsStored(t *testing.T) {
	want := record()

	if err := save(t, want); err != nil {
		t.Fatal(err)
	}

	got, found := stored(t, want.CorrelationID)
	want.After["entries"] = []any{map[string]any{"amount": float64(100)}} // how a number comes back from jsonb
	if !found || !reflect.DeepEqual(got, want) {
		t.Fatalf("stored record = %+v (found %v), want %+v", got, found, want)
	}
}

// TestPersonalDataIsRejected: a Before or an After with a personal data key, nested too, is rejected with
// ErrPersonalData and nothing is stored.
func TestPersonalDataIsRejected(t *testing.T) {
	for _, key := range []string{"name", "document_number", "email", "phone"} {
		cases := map[string]func(r *audit.Record){
			"after":         func(r *audit.Record) { r.After[key] = "x" },
			"nested after":  func(r *audit.Record) { r.After["party"] = map[string]any{key: "x"} },
			"after in list": func(r *audit.Record) { r.After["parties"] = []any{map[string]any{key: "x"}} },
			"before":        func(r *audit.Record) { r.Before[key] = "x" },
		}
		for name, mutate := range cases {
			t.Run(key+"/"+name, func(t *testing.T) {
				r := record()
				mutate(&r)

				err := save(t, r)

				if _, found := stored(t, r.CorrelationID); !errors.Is(err, audit.ErrPersonalData) || found {
					t.Fatalf("Record = %v (stored %v), want ErrPersonalData and nothing stored", err, found)
				}
			})
		}
	}
}

// TestRecordsAreImmutable: aluna_app cannot change or delete a record, and the trigger prevents the table
// owner from doing it too.
func TestRecordsAreImmutable(t *testing.T) {
	r := record()
	if err := save(t, r); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE audit.records SET result = 'other' WHERE correlation_id = $1`,
		`DELETE FROM audit.records WHERE correlation_id = $1`,
	} {
		if _, err := testPool.Exec(t.Context(), stmt, r.CorrelationID); sqlState(err) != "42501" {
			t.Errorf("%s as aluna_app = %v, want permission denied", stmt, err)
		}
		if state := ownerSQLState(t, stmt, r.CorrelationID); state != "2F003" {
			t.Errorf("%s as owner = SQLSTATE %q, want the trigger (2F003)", stmt, state)
		}
	}
	if state := ownerSQLState(t, `TRUNCATE audit.records`); state != "2F003" {
		t.Errorf("TRUNCATE as owner = SQLSTATE %q, want the trigger (2F003)", state)
	}
	if _, found := stored(t, r.CorrelationID); !found {
		t.Fatal("the record disappeared")
	}
}

// ownerSQLState runs stmt as the owner in its own transaction and returns the SQLSTATE it failed with.
func ownerSQLState(t *testing.T, stmt string, args ...any) string {
	t.Helper()
	var state string
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), "SAVEPOINT attempt"); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), stmt, args...)
		state = sqlState(err)
		_, rbErr := tx.Exec(t.Context(), "ROLLBACK TO SAVEPOINT attempt")
		return rbErr
	})
	return state
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
