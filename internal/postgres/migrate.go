package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/DrewGGM/aluna/migrations"
)

// ErrUnknownDirection indica una dirección de migración distinta de up, down o status.
var ErrUnknownDirection = errors.New("dirección de migración desconocida")

// migrationLockTimeout acota la espera por un lock: un ALTER TABLE que espera un ACCESS EXCLUSIVE deja
// en cola todo el tráfico de la aplicación detrás de él, y es mejor que la migración falle y se repita.
const migrationLockTimeout = "10s"

// OpenForMigrations abre la conexión database/sql que usa goose. No lleva el statement_timeout de la
// aplicación (crear un índice sobre una tabla grande puede tardar más de 5 s), pero sí un lock_timeout.
func OpenForMigrations(dsn string) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("configuración de la base: %w", err)
	}
	cfg.RuntimeParams["statement_timeout"] = "0" // también por encima de un default del servidor o del rol
	cfg.RuntimeParams["lock_timeout"] = migrationLockTimeout
	cfg.RuntimeParams["application_name"] = "aluna-migrate"
	return stdlib.OpenDB(*cfg), nil
}

var directions = map[string]func(context.Context, *goose.Provider, io.Writer) error{
	"up":     migrateUp,
	"down":   migrateDown,
	"status": migrateStatus,
}

// ValidDirection dice si direction es up, down o status.
func ValidDirection(direction string) bool {
	_, ok := directions[direction]
	return ok
}

// Migrate aplica las migraciones embebidas: up aplica todas las pendientes, down revierte la última
// y status lista el estado de cada una. Escribe una línea por migración en out.
func Migrate(ctx context.Context, db *sql.DB, direction string, out io.Writer) error {
	run, ok := directions[direction]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownDirection, direction)
	}
	// Dos réplicas que arrancan con aluna migrate up a la vez se turnan con un advisory lock.
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("preparar el lock de migraciones: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("preparar las migraciones: %w", err)
	}
	if err := run(ctx, provider, out); err != nil {
		return fmt.Errorf("migrate %s: %w", direction, err)
	}
	return nil
}

func migrateUp(ctx context.Context, p *goose.Provider, out io.Writer) error {
	results, err := p.Up(ctx)
	var partial *goose.PartialError
	if errors.As(err, &partial) { // goose no devuelve resultados si falla a mitad: los aplicados vienen aquí
		results = partial.Applied
	}
	printResults(out, results...)
	return err
}

func migrateDown(ctx context.Context, p *goose.Provider, out io.Writer) error {
	result, err := p.Down(ctx)
	if result != nil { // goose devuelve nil junto con el error si no llegó a revertir nada
		printResults(out, result)
	}
	return err
}

func migrateStatus(ctx context.Context, p *goose.Provider, out io.Writer) error {
	statuses, err := p.Status(ctx)
	for _, s := range statuses {
		fmt.Fprintf(out, "%-8s %s\n", s.State, s.Source.Path) //nolint:errcheck // salida informativa de la CLI
	}
	return err
}

func printResults(out io.Writer, results ...*goose.MigrationResult) {
	for _, r := range results {
		fmt.Fprintf(out, "%-8s %s (%v)\n", r.Direction, r.Source.Path, r.Duration) //nolint:errcheck // salida informativa de la CLI
	}
}
