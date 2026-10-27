//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

var testPool *pgxpool.Pool

// TestMain usa ALUNA_TEST_DATABASE_URL si existe (CI) y si no levanta PostgreSQL 16 con testcontainers.
func TestMain(m *testing.M) {
	os.Exit(runWithDatabase(m))
}

func runWithDatabase(m *testing.M) int {
	ctx := context.Background()
	dsn := os.Getenv("ALUNA_TEST_DATABASE_URL")
	if dsn == "" {
		container, err := tcpostgres.Run(ctx, "postgres:16",
			tcpostgres.WithDatabase("aluna_test"), tcpostgres.WithUsername("aluna"), tcpostgres.WithPassword("aluna"),
			tcpostgres.BasicWaitStrategies())
		if err != nil {
			fmt.Fprintln(os.Stderr, "levantar PostgreSQL:", err)
			return 1
		}
		defer container.Terminate(ctx) //nolint:errcheck // el contenedor lo recoge Ryuk si falla
		if dsn, err = container.ConnectionString(ctx, "sslmode=disable"); err != nil {
			fmt.Fprintln(os.Stderr, "dirección de PostgreSQL:", err)
			return 1
		}
	}
	pool, err := Open(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS uow_probe; CREATE TABLE uow_probe (id int PRIMARY KEY)`); err != nil {
		fmt.Fprintln(os.Stderr, "crear tabla de prueba:", err)
		return 1
	}
	testPool = pool
	return m.Run()
}

// cleanProbes vacía la tabla al terminar cada prueba, así las pruebas se pueden repetir con -count.
func cleanProbes(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := testPool.Exec(context.Background(), `TRUNCATE uow_probe`); err != nil {
			t.Error(err)
		}
	})
}

func insertProbe(ctx context.Context, tx Tx, id int) error {
	_, err := tx.Exec(ctx, `INSERT INTO uow_probe (id) VALUES ($1)`, id)
	return err
}

func probeExists(t *testing.T, id int) bool {
	t.Helper()
	var exists bool
	err := testPool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM uow_probe WHERE id = $1)`, id).Scan(&exists)
	if err != nil {
		t.Fatal(err)
	}
	return exists
}

func TestOpenSetsSessionLimits(t *testing.T) {
	want := map[string]string{"statement_timeout": "5s", "lock_timeout": "2s", "application_name": "aluna"}
	for setting, value := range want {
		var got string
		if err := testPool.QueryRow(context.Background(), "SHOW "+setting).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != value {
			t.Errorf("%s = %q, se esperaba %q", setting, got, value)
		}
	}
}

func TestDoCommitsAndUsesReadCommitted(t *testing.T) {
	cleanProbes(t)
	ctx := context.Background()
	var isolation string

	err := NewUnitOfWork(testPool).Do(ctx, func(tx Tx) error {
		if err := tx.QueryRow(ctx, "SHOW transaction_isolation").Scan(&isolation); err != nil {
			return err
		}
		return insertProbe(ctx, tx, 1)
	})
	if err != nil {
		t.Fatal(err)
	}
	if isolation != "read committed" {
		t.Errorf("aislamiento = %q, se esperaba read committed", isolation)
	}
	if !probeExists(t, 1) {
		t.Error("el commit no quedó visible")
	}
}

func TestDoRollsBackOnError(t *testing.T) {
	cleanProbes(t)
	ctx := context.Background()
	errBusiness := errors.New("saldo insuficiente")

	err := NewUnitOfWork(testPool).Do(ctx, func(tx Tx) error {
		if err := insertProbe(ctx, tx, 2); err != nil {
			return err
		}
		return errBusiness
	})

	if !errors.Is(err, errBusiness) {
		t.Fatalf("error = %v, se esperaba el de fn", err)
	}
	if probeExists(t, 2) {
		t.Error("el rollback dejó la fila")
	}
}

func TestDoRollsBackOnPanicAndRepanics(t *testing.T) {
	cleanProbes(t)
	ctx := context.Background()
	defer func() {
		if got := recover(); got != "boom" {
			t.Fatalf("panic = %v, se esperaba que Do relanzara \"boom\"", got)
		}
		if probeExists(t, 3) {
			t.Error("el rollback dejó la fila")
		}
	}()

	err := NewUnitOfWork(testPool).Do(ctx, func(tx Tx) error {
		if err := insertProbe(ctx, tx, 3); err != nil {
			return err
		}
		panic("boom")
	})
	t.Fatalf("Do debió relanzar el panic; devolvió %v", err)
}

func TestDoRetriesDeadlockOnce(t *testing.T) {
	cleanProbes(t)
	ctx := context.Background()
	calls := 0

	err := NewUnitOfWork(testPool).Do(ctx, func(tx Tx) error {
		calls++
		if err := insertProbe(ctx, tx, 4); err != nil {
			return err
		}
		if calls == 1 {
			return fmt.Errorf("débito: %w", &pgconn.PgError{Code: "40P01"})
		}
		return nil
	})

	if err != nil || calls != 2 {
		t.Fatalf("error = %v, intentos = %d; se esperaba éxito en el segundo", err, calls)
	}
	if !probeExists(t, 4) {
		t.Error("el reintento no quedó visible")
	}
}

func TestDoGivesUpAfterThreeAttempts(t *testing.T) {
	calls := 0

	err := NewUnitOfWork(testPool).Do(context.Background(), func(Tx) error {
		calls++
		return &pgconn.PgError{Code: "40001"}
	})

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "40001" || calls != maxAttempts {
		t.Fatalf("error = %v, intentos = %d; se esperaban %d intentos y el 40001", err, calls, maxAttempts)
	}
}

func TestDoDoesNotRetryOtherErrors(t *testing.T) {
	calls := 0

	err := NewUnitOfWork(testPool).Do(context.Background(), func(Tx) error {
		calls++
		return &pgconn.PgError{Code: "23505"} // unique_violation
	})

	if err == nil || calls != 1 {
		t.Fatalf("error = %v, intentos = %d; una violación de unicidad no se reintenta", err, calls)
	}
}

// TestDoRollsBackWhenFnExitsGoroutine prueba el caso de un t.Fatal dentro de fn: runtime.Goexit no es
// un panic, y la transacción no puede quedar abierta reteniendo la conexión.
func TestDoRollsBackWhenFnExitsGoroutine(t *testing.T) {
	cleanProbes(t)
	ctx := context.Background()
	done := make(chan struct{})

	go func() {
		defer close(done)
		err := NewUnitOfWork(testPool).Do(ctx, func(tx Tx) error {
			if err := insertProbe(ctx, tx, 5); err != nil {
				return err
			}
			runtime.Goexit()
			return nil
		})
		t.Errorf("Do no debió volver: fn terminó la goroutine (devolvió %v)", err)
	}()
	<-done

	if probeExists(t, 5) {
		t.Error("el rollback dejó la fila")
	}
	if acquired := testPool.Stat().AcquiredConns(); acquired != 0 {
		t.Errorf("quedaron %d conexiones tomadas del pool", acquired)
	}
}
