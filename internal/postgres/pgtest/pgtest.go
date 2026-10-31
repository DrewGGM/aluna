//go:build integration || e2e

// Package pgtest sets up the database for a package's integration and E2E tests. Each package gets its
// own database: in CI packages run in parallel against the same server and must not see each other's
// data or migrations.
package pgtest

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/DrewGGM/aluna/internal/postgres"
)

// Run is the body of TestMain: it creates the database, leaves in *pool a pool that connects as
// aluna_app (so a migration missing its GRANT fails in the tests and not in production), runs the tests
// and drops the database. The server is ALUNA_TEST_DATABASE_URL (CI) or a testcontainers PostgreSQL 16.
//
//	func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, &testPool)) }
func Run(m *testing.M, pool **pgxpool.Pool) int {
	ctx := context.Background()
	dsn, cleanup, err := Start(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pgtest:", err)
		return 1
	}
	defer cleanup()
	if *pool, err = openAsApp(ctx, dsn); err != nil {
		fmt.Fprintln(os.Stderr, "pgtest:", err)
		return 1
	}
	defer (*pool).Close()
	return m.Run()
}

// Start creates a dedicated, already migrated database and returns its DSN, with the user that owns the
// tables (not aluna_app: whoever uses it does not exercise the GRANTs), and the function that drops it.
// It is for tests that need the DSN instead of a pool.
func Start(ctx context.Context) (string, func(), error) {
	dsn, cleanup, err := StartEmpty(ctx)
	if err != nil {
		return "", nil, err
	}
	if err := migrate(ctx, dsn); err != nil {
		cleanup()
		return "", nil, err
	}
	return dsn, cleanup, nil
}

// StartEmpty is Start without migrating, for when the migration itself is under test. The function it
// returns drops the database and stops the container even if ctx is already canceled, as happens with
// t.Context() in t.Cleanup.
func StartEmpty(ctx context.Context) (string, func(), error) {
	serverDSN, stop, err := server(ctx)
	if err != nil {
		return "", nil, err
	}
	db, err := createDatabase(ctx, serverDSN)
	if err != nil {
		stop()
		return "", nil, err
	}
	ctx = context.WithoutCancel(ctx)
	return db.dsn, func() { db.drop(ctx); stop() }, nil
}

func server(ctx context.Context) (string, func(), error) {
	if dsn := os.Getenv("ALUNA_TEST_DATABASE_URL"); dsn != "" {
		return dsn, func() {}, nil
	}
	container, err := tcpostgres.Run(ctx, "postgres:16",
		tcpostgres.WithDatabase("aluna_test"), tcpostgres.WithUsername("aluna"), tcpostgres.WithPassword("aluna"),
		tcpostgres.BasicWaitStrategies())
	if err != nil {
		return "", nil, fmt.Errorf("start PostgreSQL: %w", err)
	}
	stop := func() {
		if err := container.Terminate(context.WithoutCancel(ctx)); err != nil {
			fmt.Fprintln(os.Stderr, "pgtest: stop container (Ryuk will reap it):", err)
		}
	}
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		stop()
		return "", nil, fmt.Errorf("PostgreSQL address: %w", err)
	}
	return dsn, stop, nil
}

type database struct {
	name, serverDSN, dsn string
}

// createDatabase creates an empty database with a random name (only [a-z2-7]: no injection possible).
func createDatabase(ctx context.Context, serverDSN string) (*database, error) {
	db := &database{name: "aluna_test_" + strings.ToLower(rand.Text()), serverDSN: serverDSN}
	var err error
	if db.dsn, err = withDatabase(serverDSN, db.name); err != nil {
		return nil, err
	}
	if err := db.exec(ctx, "CREATE DATABASE "+db.name); err != nil {
		return nil, fmt.Errorf("create database %s: %w", db.name, err)
	}
	return db, nil
}

func openAsApp(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	appDSN, err := AppDSN(dsn)
	if err != nil {
		return nil, err
	}
	return postgres.Open(ctx, appDSN)
}

// AsOwner runs fn in a transaction as the tables' owner (SET LOCAL ROLE NONE on Run's pool), to seed or
// age what aluna_app cannot write: entities, periods, creation dates. An error stops the test.
func AsOwner(t testing.TB, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) {
	t.Helper()
	err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), "SET LOCAL ROLE NONE"); err != nil {
			return err
		}
		return fn(tx)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// AppDSN turns the owner's DSN into one that connects with the aluna_app role, like the real process.
func AppDSN(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", errors.New("invalid test database DSN")
	}
	query := u.Query()
	query.Set("options", "-c role=aluna_app")
	u.RawQuery = strings.ReplaceAll(query.Encode(), "+", "%20") // pgx does not read "+" as a space
	return u.String(), nil
}

// drop drops the database. A failure is reported: with a shared server the database would stay alive.
func (db *database) drop(ctx context.Context) {
	if err := db.exec(ctx, "DROP DATABASE IF EXISTS "+db.name+" WITH (FORCE)"); err != nil {
		fmt.Fprintf(os.Stderr, "pgtest: drop database %s: %v\n", db.name, err)
	}
}

func (db *database) exec(ctx context.Context, sql string) error {
	conn, err := pgx.Connect(ctx, db.serverDSN)
	if err != nil {
		return fmt.Errorf("connect to server: %w", err)
	}
	_, err = conn.Exec(ctx, sql)
	return errors.Join(err, conn.Close(ctx))
}

// withDatabase changes the database of a postgres:// DSN. It leaves the DSN out of the error: it carries
// the password.
func withDatabase(serverDSN, name string) (string, error) {
	u, err := url.Parse(serverDSN)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return "", errors.New("test server DSN must be a postgres:// URL")
	}
	u.Path = "/" + name
	return u.String(), nil
}

func migrate(ctx context.Context, dsn string) error {
	db, err := postgres.OpenForMigrations(dsn)
	if err != nil {
		return err
	}
	return errors.Join(postgres.Migrate(ctx, db, "up", io.Discard), db.Close())
}
