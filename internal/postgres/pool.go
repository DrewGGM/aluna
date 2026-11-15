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
	return OpenWithConnections(ctx, dsn, 0)
}

// OpenWithConnections is Open with at least conns connections in the pool, for a process whose work at once needs
// more than pgx's default (the larger of 4 and the CPUs, or dsn's pool_max_conns).
func OpenWithConnections(ctx context.Context, dsn string, conns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("database config: %w", err)
	}
	cfg.MaxConns = max(cfg.MaxConns, conns)
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
