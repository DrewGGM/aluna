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

// ErrUnknownDirection means a migration direction other than up, down or status.
var ErrUnknownDirection = errors.New("unknown migration direction")

// migrationLockTimeout bounds the wait for a lock: an ALTER TABLE waiting for an ACCESS EXCLUSIVE queues
// all the application's traffic behind it, and it is better for the migration to fail and be retried.
const migrationLockTimeout = "10s"

// OpenForMigrations opens the database/sql connection goose uses. It does not carry the application's
// statement_timeout (creating an index on a large table can take more than 5 s), but it does carry a
// lock_timeout.
func OpenForMigrations(dsn string) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("database config: %w", err)
	}
	cfg.RuntimeParams["statement_timeout"] = "0" // also overrides a server or role default
	cfg.RuntimeParams["lock_timeout"] = migrationLockTimeout
	cfg.RuntimeParams["application_name"] = "aluna-migrate"
	return stdlib.OpenDB(*cfg), nil
}

var directions = map[string]func(context.Context, *goose.Provider, io.Writer) error{
	"up":     migrateUp,
	"down":   migrateDown,
	"status": migrateStatus,
}

// ValidDirection reports whether direction is up, down or status.
func ValidDirection(direction string) bool {
	_, ok := directions[direction]
	return ok
}

// Migrate applies the embedded migrations: up applies all pending ones, down reverts the last one and
// status lists the state of each. It writes one line per migration to out.
func Migrate(ctx context.Context, db *sql.DB, direction string, out io.Writer) error {
	run, ok := directions[direction]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownDirection, direction)
	}
	// Two replicas starting with aluna migrate up at the same time take turns with an advisory lock.
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("prepare migration lock: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("prepare migrations: %w", err)
	}
	if err := run(ctx, provider, out); err != nil {
		return fmt.Errorf("migrate %s: %w", direction, err)
	}
	return nil
}

func migrateUp(ctx context.Context, p *goose.Provider, out io.Writer) error {
	results, err := p.Up(ctx)
	var partial *goose.PartialError
	if errors.As(err, &partial) { // goose returns no results if it fails midway: the applied ones come here
		results = partial.Applied
	}
	printResults(out, results...)
	return err
}

func migrateDown(ctx context.Context, p *goose.Provider, out io.Writer) error {
	result, err := p.Down(ctx)
	if result != nil { // goose returns nil along with the error if it reverted nothing
		printResults(out, result)
	}
	return err
}

func migrateStatus(ctx context.Context, p *goose.Provider, out io.Writer) error {
	statuses, err := p.Status(ctx)
	for _, s := range statuses {
		fmt.Fprintf(out, "%-8s %s\n", s.State, s.Source.Path) //nolint:errcheck // informational CLI output
	}
	return err
}

func printResults(out io.Writer, results ...*goose.MigrationResult) {
	for _, r := range results {
		fmt.Fprintf(out, "%-8s %s (%v)\n", r.Direction, r.Source.Path, r.Duration) //nolint:errcheck // informational CLI output
	}
}
