// Package postgres es la infraestructura de PostgreSQL (ADR-0002, ADR-0009): pool de conexiones y
// Unit of Work. Las fachadas de los módulos reciben su Tx; solo los adapters ejecutan SQL con ella.
package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// sessionLimits se fijan en cada conexión (ADR-0004): una consulta o un lock que se cuelga falla en
// segundos en vez de retener la conexión.
var sessionLimits = map[string]string{
	"statement_timeout": "5s",
	"lock_timeout":      "2s",
	"application_name":  "aluna",
}

// Open abre el pool contra dsn y comprueba la conexión.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("configuración de la base: %w", err)
	}
	for name, value := range sessionLimits {
		cfg.ConnConfig.RuntimeParams[name] = value
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("crear el pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("conectar a la base: %w", err)
	}
	return pool, nil
}
