// Package postgres guarda los registros de auditoría en audit.records y sus bloques en audit.blocks.
package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/audit/internal/app"
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

// Store es la cadena de bloques de auditoría sobre q: una Tx para sellar, el pool o una Tx para verificar.
type Store struct{ q postgres.Querier }

// NewStore crea el Store sobre q.
func NewStore(q postgres.Querier) Store { return Store{q: q} }

const recordColumns = `seq, recorded_at, actor, role, channel, ip, correlation_id, command, targets, result, before::text, after::text`

// Unsealed bloquea hasta limit registros confirmados sin bloque, en orden de seq.
func (s Store) Unsealed(ctx context.Context, limit int) ([]app.Record, error) {
	return s.records(ctx, `SELECT `+recordColumns+` FROM audit.records WHERE block_id IS NULL ORDER BY seq LIMIT $1 FOR UPDATE`, limit)
}

// RecordsOf son los registros del bloque, en orden de seq.
func (s Store) RecordsOf(ctx context.Context, blockID int64) ([]app.Record, error) {
	return s.records(ctx, `SELECT `+recordColumns+` FROM audit.records WHERE block_id = $1 ORDER BY seq`, blockID)
}

func (s Store) records(ctx context.Context, query string, args ...any) ([]app.Record, error) {
	rows, err := s.q.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("leer registros de auditoría: %w", err)
	}
	records, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (app.Record, error) {
		var r app.Record
		var before, after *string
		err := row.Scan(&r.Seq, &r.RecordedAt, &r.Actor, &r.Role, &r.Channel, &r.IP, &r.CorrelationID, &r.Command,
			&r.Targets, &r.Result, &before, &after)
		r.Before, r.After = raw(before), raw(after)
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("leer registros de auditoría: %w", err)
	}
	return records, nil
}

// raw es el JSON de una columna jsonb, nil si es NULL.
func raw(s *string) json.RawMessage {
	if s == nil {
		return nil
	}
	return json.RawMessage(*s)
}

const blockColumns = `id, prev_hash, hash, records`

// LastBlock es el último bloque, o app.Block{} si no hay.
func (s Store) LastBlock(ctx context.Context) (app.Block, error) {
	return s.block(ctx, `SELECT `+blockColumns+` FROM audit.blocks ORDER BY id DESC LIMIT 1`)
}

// BlockBefore es el bloque anterior a id, o app.Block{} si no hay.
func (s Store) BlockBefore(ctx context.Context, id int64) (app.Block, error) {
	return s.block(ctx, `SELECT `+blockColumns+` FROM audit.blocks WHERE id < $1 ORDER BY id DESC LIMIT 1`, id)
}

func (s Store) block(ctx context.Context, query string, args ...any) (app.Block, error) {
	blocks, err := s.blocks(ctx, query, args...)
	if err != nil || len(blocks) == 0 {
		return app.Block{}, err
	}
	return blocks[0], nil
}

// Blocks son los bloques con id entre from y to, en orden.
func (s Store) Blocks(ctx context.Context, from, to int64) ([]app.Block, error) {
	return s.blocks(ctx, `SELECT `+blockColumns+` FROM audit.blocks WHERE id BETWEEN $1 AND $2 ORDER BY id`, from, to)
}

func (s Store) blocks(ctx context.Context, query string, args ...any) ([]app.Block, error) {
	rows, err := s.q.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("leer bloques de auditoría: %w", err)
	}
	blocks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (app.Block, error) {
		var b app.Block
		var prev, hash []byte
		err := row.Scan(&b.ID, &prev, &hash, &b.Records)
		copy(b.PrevHash[:], prev)
		copy(b.Hash[:], hash)
		return b, err
	})
	if err != nil {
		return nil, fmt.Errorf("leer bloques de auditoría: %w", err)
	}
	return blocks, nil
}

// InsertBlock guarda un bloque y devuelve su id. Otro bloque con el mismo prevHash viola la unicidad.
func (s Store) InsertBlock(ctx context.Context, prevHash, hash [32]byte, records int) (int64, error) {
	var id int64
	err := s.q.QueryRow(ctx, `INSERT INTO audit.blocks (prev_hash, hash, records) VALUES ($1, $2, $3) RETURNING id`,
		prevHash[:], hash[:], records).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("guardar el bloque de auditoría: %w", err)
	}
	return id, nil
}

// AssignBlock marca con blockID los registros seqs.
func (s Store) AssignBlock(ctx context.Context, blockID int64, seqs []int64) error {
	if _, err := s.q.Exec(ctx, `UPDATE audit.records SET block_id = $1 WHERE seq = ANY($2)`, blockID, seqs); err != nil {
		return fmt.Errorf("sellar registros en el bloque %d: %w", blockID, err)
	}
	return nil
}
