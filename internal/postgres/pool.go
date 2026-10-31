// Package postgres is the PostgreSQL infrastructure (ADR-0002, ADR-0009): connection pool and Unit of
// Work. Module facades receive its Tx; only adapters run SQL with it.
package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// sessionLimits are set on every connection (ADR-0004): a query or a lock that hangs fails within
// seconds instead of holding the connection.
var sessionLimits = map[string]string{
	"statement_timeout": "5s",
	"lock_timeout":      "2s",
	"application_name":  "aluna",
}

// Open opens the pool against dsn and checks the connection.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("database config: %w", err)
	}
	for name, value := range sessionLimits {
		cfg.ConnConfig.RuntimeParams[name] = value
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return pool, nil
}
