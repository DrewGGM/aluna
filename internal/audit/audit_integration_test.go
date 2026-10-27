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
		Actor: "operador", Role: "tesorero", Channel: "api", IP: "10.0.0.7", CorrelationID: ids.New().String(),
		Command: "POST /v1/transactions", Targets: []string{ids.New().String()}, Result: "ok",
		Before: map[string]any{"status": "pending"}, After: map[string]any{"status": "posted", "entries": []any{map[string]any{"amount": 100}}},
	}
}

func save(t *testing.T, r audit.Record) error {
	t.Helper()
	return postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error { return audit.New().Record(t.Context(), tx, r) })
}

// stored lee como dueño (aluna_app solo inserta) el registro de la correlación.
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

// TestARecordIsStored: Record guarda el registro completo en la transacción que recibe.
func TestARecordIsStored(t *testing.T) {
	want := record()

	if err := save(t, want); err != nil {
		t.Fatal(err)
	}

	got, found := stored(t, want.CorrelationID)
	want.After["entries"] = []any{map[string]any{"amount": float64(100)}} // así vuelve un número de jsonb
	if !found || !reflect.DeepEqual(got, want) {
		t.Fatalf("registro guardado = %+v (encontrado %v), se esperaba %+v", got, found, want)
	}
}

// TestPersonalDataIsRejected: un Before o un After con una clave de datos personales, también anidada, se
// rechaza con ErrPersonalData y no se guarda nada.
func TestPersonalDataIsRejected(t *testing.T) {
	for _, key := range []string{"name", "document_number", "email", "phone"} {
		cases := map[string]func(r *audit.Record){
			"after":          func(r *audit.Record) { r.After[key] = "x" },
			"after anidado":  func(r *audit.Record) { r.After["party"] = map[string]any{key: "x"} },
			"after en lista": func(r *audit.Record) { r.After["parties"] = []any{map[string]any{key: "x"}} },
			"before":         func(r *audit.Record) { r.Before[key] = "x" },
		}
		for name, mutate := range cases {
			t.Run(key+"/"+name, func(t *testing.T) {
				r := record()
				mutate(&r)

				err := save(t, r)

				if _, found := stored(t, r.CorrelationID); !errors.Is(err, audit.ErrPersonalData) || found {
					t.Fatalf("Record = %v (guardado %v), se esperaba ErrPersonalData y nada guardado", err, found)
				}
			})
		}
	}
}

// TestRecordsAreImmutable: aluna_app no puede cambiar ni borrar un registro, y el trigger se lo impide
// también al dueño de las tablas.
func TestRecordsAreImmutable(t *testing.T) {
	r := record()
	if err := save(t, r); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE audit.records SET result = 'otro' WHERE correlation_id = $1`,
		`DELETE FROM audit.records WHERE correlation_id = $1`,
	} {
		if _, err := testPool.Exec(t.Context(), stmt, r.CorrelationID); sqlState(err) != "42501" {
			t.Errorf("%s como aluna_app = %v, se esperaba permiso denegado", stmt, err)
		}
		if state := ownerSQLState(t, stmt, r.CorrelationID); state != "2F003" {
			t.Errorf("%s como dueño = SQLSTATE %q, se esperaba el trigger (2F003)", stmt, state)
		}
	}
	if state := ownerSQLState(t, `TRUNCATE audit.records`); state != "2F003" {
		t.Errorf("TRUNCATE como dueño = SQLSTATE %q, se esperaba el trigger (2F003)", state)
	}
	if _, found := stored(t, r.CorrelationID); !found {
		t.Fatal("el registro desapareció")
	}
}

// ownerSQLState corre stmt como dueño en su propia transacción y devuelve el SQLSTATE con que falló.
func ownerSQLState(t *testing.T, stmt string, args ...any) string {
	t.Helper()
	var state string
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), "SAVEPOINT intento"); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), stmt, args...)
		state = sqlState(err)
		_, rbErr := tx.Exec(t.Context(), "ROLLBACK TO SAVEPOINT intento")
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
