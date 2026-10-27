// Package postgres guarda los registros de auditoría en audit.records.
package postgres

import (
	"context"
	"fmt"

	"github.com/DrewGGM/aluna/internal/postgres"
)

// Row es un registro tal como se inserta, con los campos de audit.Record en el mismo orden (el adapter no
// puede importar la fachada); seq, recorded_at y block_id los pone la base.
type Row struct {
	Actor, Role, Channel, IP string
	CorrelationID, Command   string
	Targets                  []string
	Result                   string
	Before, After            map[string]any
}

// Insert guarda row dentro de tx.
func Insert(ctx context.Context, tx postgres.Tx, row Row) error {
	targets := row.Targets
	if targets == nil {
		targets = []string{} // la columna es NOT NULL: sin IDs es una lista vacía
	}
	_, err := tx.Exec(ctx, `INSERT INTO audit.records
		(actor, role, channel, ip, correlation_id, command, targets, result, before, after)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		row.Actor, row.Role, row.Channel, row.IP, row.CorrelationID, row.Command, targets, row.Result, row.Before, row.After)
	if err != nil {
		return fmt.Errorf("guardar el registro de auditoría de %s: %w", row.Command, err)
	}
	return nil
}
