//go:build integration

package postgres

import (
	"bytes"
	"context"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/migrations"
)

var moduleSchemas = []string{
	"infra", "ledger", "calendar", "party", "products", "pricing", "deposits", "payments",
	"compliance", "recon", "accounting", "approvals", "audit", "access", "webhooks",
}

func countSchemas(t *testing.T) int {
	t.Helper()
	var n int
	err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.schemata WHERE schema_name = ANY($1)`, moduleSchemas).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// migrate runs a direction through the same path as aluna migrate and returns the pending migrations
// according to status.
func migrate(t *testing.T, direction string) []string {
	t.Helper()
	db, err := OpenForMigrations(testPool.Config().ConnConfig.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	ctx := context.Background()
	var out, status bytes.Buffer
	if err := Migrate(ctx, db, direction, &out); err != nil {
		t.Fatalf("migrate %s: %v\n%s", direction, err, out.String())
	}
	if err := Migrate(ctx, db, "status", &status); err != nil {
		t.Fatalf("migrate status: %v", err)
	}
	var pending []string
	for line := range strings.Lines(status.String()) {
		if state, file, ok := strings.Cut(strings.TrimSpace(line), " "); ok && state == "pending" {
			pending = append(pending, strings.TrimSpace(file))
		}
	}
	return pending
}

func lastMigration(t *testing.T) string {
	t.Helper()
	files, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("embedded migrations: %v %v", files, err)
	}
	return slices.Max(files)
}

func TestMigrateUpStatusDownUp(t *testing.T) {
	if pending := migrate(t, "up"); len(pending) != 0 {
		t.Fatalf("pending after up: %v", pending)
	}
	if n := countSchemas(t); n != len(moduleSchemas) {
		t.Fatalf("after up there are %d of %d schemas", n, len(moduleSchemas))
	}

	if pending := migrate(t, "down"); !slices.Equal(pending, []string{lastMigration(t)}) {
		t.Fatalf("down must leave only %s pending, got %v", lastMigration(t), pending)
	}

	if pending := migrate(t, "up"); len(pending) != 0 {
		t.Fatalf("pending after the second up: %v", pending)
	}
}

func TestAppRoleOwnsNothingAndCannotLogIn(t *testing.T) {
	migrate(t, "up")
	ctx := context.Background()
	var canLogin, ownsSchemas bool
	if err := testPool.QueryRow(ctx, `SELECT rolcanlogin FROM pg_roles WHERE rolname = 'aluna_app'`).Scan(&canLogin); err != nil {
		t.Fatalf("the aluna_app role must exist: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspowner = 'aluna_app'::regrole)`).Scan(&ownsSchemas); err != nil {
		t.Fatal(err)
	}
	if canLogin || ownsSchemas {
		t.Fatalf("aluna_app: login = %v, owns schemas = %v; want neither", canLogin, ownsSchemas)
	}
}

func TestOpenForMigrationsHasNoStatementTimeout(t *testing.T) {
	db, err := OpenForMigrations(testPool.Config().ConnConfig.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck // close in test
	want := map[string]string{"statement_timeout": "0", "lock_timeout": "10s"}
	for setting, value := range want {
		var got string
		if err := db.QueryRowContext(context.Background(), "SHOW "+setting).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != value {
			t.Errorf("%s = %q, want %q", setting, got, value)
		}
	}
}

// TestAppRoleCreationToleratesConcurrentDatabases runs the 0001 block that creates the role in two
// sessions at once, as happens when several databases on the same server migrate in parallel.
func TestAppRoleCreationToleratesConcurrentDatabases(t *testing.T) {
	ctx := context.Background()
	createRole := roleBlockOf(t, "0001_schemas.sql")
	if _, err := testPool.Exec(ctx, `DROP ROLE IF EXISTS aluna_concurrency_probe`); err != nil {
		t.Fatal(err)
	}
	probe := strings.ReplaceAll(createRole, "aluna_app", "aluna_concurrency_probe")
	t.Cleanup(func() {
		if _, err := testPool.Exec(ctx, `DROP ROLE IF EXISTS aluna_concurrency_probe`); err != nil {
			t.Error(err)
		}
	})
	first, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback(ctx) //nolint:errcheck // committed below; if the test fails earlier, it rolls back
	if _, err := first.Exec(ctx, probe); err != nil {
		t.Fatal(err)
	}
	second := make(chan error, 1)
	go func() { // waits for the first session's pg_authid lock and gets unique_violation
		_, err := testPool.Exec(ctx, probe)
		second <- err
	}()
	time.Sleep(200 * time.Millisecond) // the second session is left waiting
	if err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if err := <-second; err != nil {
		t.Fatalf("the second database must not fail when creating a role that already exists: %v", err)
	}
}

// roleBlockOf extracts from the migration the block between StatementBegin and StatementEnd, to test the
// real SQL and not a copy.
func roleBlockOf(t *testing.T, file string) string {
	t.Helper()
	sql, err := fs.ReadFile(migrations.FS, file)
	if err != nil {
		t.Fatal(err)
	}
	_, rest, okBegin := strings.Cut(string(sql), "-- +goose StatementBegin")
	block, _, okEnd := strings.Cut(rest, "-- +goose StatementEnd")
	if !okBegin || !okEnd || !strings.Contains(block, "CREATE ROLE aluna_app") {
		t.Fatalf("%s lacks the block that creates aluna_app", file)
	}
	return block
}
