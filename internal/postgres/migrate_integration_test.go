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

// migrate corre una dirección por el mismo camino que aluna migrate y devuelve las migraciones
// pendientes según status.
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
		t.Fatalf("migraciones embebidas: %v %v", files, err)
	}
	return slices.Max(files)
}

func TestMigrateUpStatusDownUp(t *testing.T) {
	if pending := migrate(t, "up"); len(pending) != 0 {
		t.Fatalf("tras up quedan pendientes: %v", pending)
	}
	if n := countSchemas(t); n != len(moduleSchemas) {
		t.Fatalf("tras up hay %d de %d esquemas", n, len(moduleSchemas))
	}

	if pending := migrate(t, "down"); !slices.Equal(pending, []string{lastMigration(t)}) {
		t.Fatalf("down debe dejar pendiente solo %s, quedó %v", lastMigration(t), pending)
	}

	if pending := migrate(t, "up"); len(pending) != 0 {
		t.Fatalf("tras el segundo up quedan pendientes: %v", pending)
	}
}

func TestAppRoleOwnsNothingAndCannotLogIn(t *testing.T) {
	migrate(t, "up")
	ctx := context.Background()
	var canLogin, ownsSchemas bool
	if err := testPool.QueryRow(ctx, `SELECT rolcanlogin FROM pg_roles WHERE rolname = 'aluna_app'`).Scan(&canLogin); err != nil {
		t.Fatalf("el rol aluna_app debe existir: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspowner = 'aluna_app'::regrole)`).Scan(&ownsSchemas); err != nil {
		t.Fatal(err)
	}
	if canLogin || ownsSchemas {
		t.Fatalf("aluna_app: login = %v, dueño de esquemas = %v; se esperaba ninguno", canLogin, ownsSchemas)
	}
}

func TestOpenForMigrationsHasNoStatementTimeout(t *testing.T) {
	db, err := OpenForMigrations(testPool.Config().ConnConfig.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck // cierre en prueba
	want := map[string]string{"statement_timeout": "0", "lock_timeout": "10s"}
	for setting, value := range want {
		var got string
		if err := db.QueryRowContext(context.Background(), "SHOW "+setting).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != value {
			t.Errorf("%s = %q, se esperaba %q", setting, got, value)
		}
	}
}

// TestAppRoleCreationToleratesConcurrentDatabases corre el bloque de 0001 que crea el rol en dos
// sesiones a la vez, como pasa cuando varias bases del mismo servidor migran en paralelo.
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
	defer first.Rollback(ctx) //nolint:errcheck // se confirma abajo; si la prueba falla antes, se revierte
	if _, err := first.Exec(ctx, probe); err != nil {
		t.Fatal(err)
	}
	second := make(chan error, 1)
	go func() { // espera el lock de pg_authid de la primera sesión y recibe unique_violation
		_, err := testPool.Exec(ctx, probe)
		second <- err
	}()
	time.Sleep(200 * time.Millisecond) // la segunda sesión queda esperando
	if err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if err := <-second; err != nil {
		t.Fatalf("la segunda base no debe fallar al crear un rol que ya existe: %v", err)
	}
}

// roleBlockOf saca de la migración el bloque entre StatementBegin y StatementEnd, para probar el SQL
// real y no una copia.
func roleBlockOf(t *testing.T, file string) string {
	t.Helper()
	sql, err := fs.ReadFile(migrations.FS, file)
	if err != nil {
		t.Fatal(err)
	}
	_, rest, okBegin := strings.Cut(string(sql), "-- +goose StatementBegin")
	block, _, okEnd := strings.Cut(rest, "-- +goose StatementEnd")
	if !okBegin || !okEnd || !strings.Contains(block, "CREATE ROLE aluna_app") {
		t.Fatalf("%s no tiene el bloque que crea aluna_app", file)
	}
	return block
}
