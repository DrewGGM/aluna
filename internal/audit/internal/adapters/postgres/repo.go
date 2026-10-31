// Package postgres stores the audit records in audit.records and their blocks in audit.blocks.
package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/audit/internal/app"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// Row is a record as it is inserted, with the fields of audit.Record in the same order (the adapter cannot
// import the facade); the database sets seq, recorded_at and block_id.
type Row struct {
	Actor, Role, Channel, IP string
	CorrelationID, Command   string
	Targets                  []string
	Result                   string
	Before, After            map[string]any
}

// Insert stores row within tx.
func Insert(ctx context.Context, tx postgres.Tx, row Row) error {
	targets := row.Targets
	if targets == nil {
		targets = []string{} // the column is NOT NULL: no IDs is an empty list
	}
	_, err := tx.Exec(ctx, `INSERT INTO audit.records
		(actor, role, channel, ip, correlation_id, command, targets, result, before, after)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		row.Actor, row.Role, row.Channel, row.IP, row.CorrelationID, row.Command, targets, row.Result, row.Before, row.After)
	if err != nil {
		return fmt.Errorf("insert audit record for %s: %w", row.Command, err)
	}
	return nil
}

// Store is the audit block chain over q: a Tx to seal, the pool or a Tx to verify.
type Store struct{ q postgres.Querier }

// NewStore creates the Store over q.
func NewStore(q postgres.Querier) Store { return Store{q: q} }

const recordColumns = `seq, recorded_at, actor, role, channel, ip, correlation_id, command, targets, result, before::text, after::text`

// Unsealed locks up to limit committed records with no block, in seq order.
func (s Store) Unsealed(ctx context.Context, limit int) ([]app.Record, error) {
	return s.records(ctx, `SELECT `+recordColumns+` FROM audit.records WHERE block_id IS NULL ORDER BY seq LIMIT $1 FOR UPDATE`, limit)
}

// RecordsOf are the records of the block, in seq order.
func (s Store) RecordsOf(ctx context.Context, blockID int64) ([]app.Record, error) {
	return s.records(ctx, `SELECT `+recordColumns+` FROM audit.records WHERE block_id = $1 ORDER BY seq`, blockID)
}

func (s Store) records(ctx context.Context, query string, args ...any) ([]app.Record, error) {
	rows, err := s.q.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read audit records: %w", err)
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
		return nil, fmt.Errorf("read audit records: %w", err)
	}
	return records, nil
}

// raw is the JSON of a jsonb column, nil if it is NULL.
func raw(s *string) json.RawMessage {
	if s == nil {
		return nil
	}
	return json.RawMessage(*s)
}

const blockColumns = `id, prev_hash, hash, records`

// LastBlock is the last block, or app.Block{} if there is none.
func (s Store) LastBlock(ctx context.Context) (app.Block, error) {
	return s.block(ctx, `SELECT `+blockColumns+` FROM audit.blocks ORDER BY id DESC LIMIT 1`)
}

// BlockBefore is the block before id, or app.Block{} if there is none.
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

// Blocks are the blocks with id between from and to, in order.
func (s Store) Blocks(ctx context.Context, from, to int64) ([]app.Block, error) {
	return s.blocks(ctx, `SELECT `+blockColumns+` FROM audit.blocks WHERE id BETWEEN $1 AND $2 ORDER BY id`, from, to)
}

func (s Store) blocks(ctx context.Context, query string, args ...any) ([]app.Block, error) {
	rows, err := s.q.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read audit blocks: %w", err)
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
		return nil, fmt.Errorf("read audit blocks: %w", err)
	}
	return blocks, nil
}

// InsertBlock stores a block and returns its id. Another block with the same prevHash violates uniqueness.
func (s Store) InsertBlock(ctx context.Context, prevHash, hash [32]byte, records int) (int64, error) {
	var id int64
	err := s.q.QueryRow(ctx, `INSERT INTO audit.blocks (prev_hash, hash, records) VALUES ($1, $2, $3) RETURNING id`,
		prevHash[:], hash[:], records).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert audit block: %w", err)
	}
	return id, nil
}

// AssignBlock marks records seqs with blockID.
func (s Store) AssignBlock(ctx context.Context, blockID int64, seqs []int64) error {
	if _, err := s.q.Exec(ctx, `UPDATE audit.records SET block_id = $1 WHERE seq = ANY($2)`, blockID, seqs); err != nil {
		return fmt.Errorf("seal records into block %d: %w", blockID, err)
	}
	return nil
}
