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

// TestMain uses ALUNA_TEST_DATABASE_URL if set (CI) and otherwise starts PostgreSQL 16 with
// testcontainers.
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
			fmt.Fprintln(os.Stderr, "start PostgreSQL:", err)
			return 1
		}
		defer container.Terminate(ctx) //nolint:errcheck // Ryuk reaps the container if this fails
		if dsn, err = container.ConnectionString(ctx, "sslmode=disable"); err != nil {
			fmt.Fprintln(os.Stderr, "PostgreSQL address:", err)
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
		fmt.Fprintln(os.Stderr, "create test table:", err)
		return 1
	}
	testPool = pool
	return m.Run()
}

// cleanProbes empties the table when each test ends, so the tests can be repeated with -count.
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
			t.Errorf("%s = %q, want %q", setting, got, value)
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
		t.Errorf("isolation = %q, want read committed", isolation)
	}
	if !probeExists(t, 1) {
		t.Error("the commit is not visible")
	}
}

func TestDoRollsBackOnError(t *testing.T) {
	cleanProbes(t)
	ctx := context.Background()
	errBusiness := errors.New("insufficient funds")

	err := NewUnitOfWork(testPool).Do(ctx, func(tx Tx) error {
		if err := insertProbe(ctx, tx, 2); err != nil {
			return err
		}
		return errBusiness
	})

	if !errors.Is(err, errBusiness) {
		t.Fatalf("error = %v, want fn's", err)
	}
	if probeExists(t, 2) {
		t.Error("the rollback left the row")
	}
}

func TestDoRollsBackOnPanicAndRepanics(t *testing.T) {
	cleanProbes(t)
	ctx := context.Background()
	defer func() {
		if got := recover(); got != "boom" {
			t.Fatalf("panic = %v, want Do to re-panic with \"boom\"", got)
		}
		if probeExists(t, 3) {
			t.Error("the rollback left the row")
		}
	}()

	err := NewUnitOfWork(testPool).Do(ctx, func(tx Tx) error {
		if err := insertProbe(ctx, tx, 3); err != nil {
			return err
		}
		panic("boom")
	})
	t.Fatalf("Do should have re-panicked; it returned %v", err)
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
			return fmt.Errorf("debit: %w", &pgconn.PgError{Code: "40P01"})
		}
		return nil
	})

	if err != nil || calls != 2 {
		t.Fatalf("error = %v, attempts = %d; want success on the second", err, calls)
	}
	if !probeExists(t, 4) {
		t.Error("the retry is not visible")
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
		t.Fatalf("error = %v, attempts = %d; want %d attempts and the 40001", err, calls, maxAttempts)
	}
}

func TestDoDoesNotRetryOtherErrors(t *testing.T) {
	calls := 0

	err := NewUnitOfWork(testPool).Do(context.Background(), func(Tx) error {
		calls++
		return &pgconn.PgError{Code: "23505"} // unique_violation
	})

	if err == nil || calls != 1 {
		t.Fatalf("error = %v, attempts = %d; a unique violation is not retried", err, calls)
	}
}

// TestDoRollsBackWhenFnExitsGoroutine covers a t.Fatal inside fn: runtime.Goexit is not a panic, and the
// transaction must not stay open holding the connection.
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
		t.Errorf("Do should not have returned: fn ended the goroutine (returned %v)", err)
	}()
	<-done

	if probeExists(t, 5) {
		t.Error("the rollback left the row")
	}
	if acquired := testPool.Stat().AcquiredConns(); acquired != 0 {
		t.Errorf("%d pool connections left acquired", acquired)
	}
}
