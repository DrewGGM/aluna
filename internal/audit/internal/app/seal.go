// Package app seals the audit log (ADR-0012): blocks chained by hash and anchored outside the database, and
// the verification of that chain.
package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Record is an audit record as it is sealed. Before and After are the JSON the database stored, nil if
// there is none.
type Record struct {
	Seq                      int64
	RecordedAt               time.Time
	Actor, Role, Channel, IP string
	CorrelationID, Command   string
	Targets                  []string
	Result                   string
	Before, After            json.RawMessage
}

// Block is a sealed block: its hash chains the previous one (PrevHash) with its records. The zero value
// means "no block": its hash, all zeros, is the PrevHash of the first one.
type Block struct {
	ID             int64
	PrevHash, Hash [32]byte
	Records        int
}

var (
	// ErrNotAnchored means a block with no anchored hash.
	ErrNotAnchored = errors.New("the block has no anchor")
	// ErrNoBlocks means a range with no sealed or anchored blocks: there is nothing to verify.
	ErrNoBlocks = errors.New("no sealed blocks in the range")
)

// AnchorStore stores the hash of each block outside the database (WORM in production): what cannot be
// rewritten from the database and what the chain is compared against. Get of a block with no anchor is
// ErrNotAnchored; Put of the same hash again does nothing.
type AnchorStore interface {
	Put(ctx context.Context, blockID int64, hash [32]byte) error
	Get(ctx context.Context, blockID int64) ([32]byte, error)
}

// SealStore is the sealing persistence, within a transaction.
type SealStore interface {
	// Unsealed locks (FOR UPDATE) up to limit committed records with no block, in seq order.
	Unsealed(ctx context.Context, limit int) ([]Record, error)
	// LastBlock is the last block, or Block{} if there is none yet.
	LastBlock(ctx context.Context) (Block, error)
	// InsertBlock stores a block; another one already sealed with the same prevHash is an error (the chain
	// does not fork: the sealer that loses the race rolls back and carries on in the next round).
	InsertBlock(ctx context.Context, prevHash, hash [32]byte, records int) (int64, error)
	AssignBlock(ctx context.Context, blockID int64, seqs []int64) error
}

// VerifyStore reads the chain to verify it.
type VerifyStore interface {
	// LastBlock is the last block, or Block{} if there is none yet.
	LastBlock(ctx context.Context) (Block, error)
	// Blocks are the blocks with id between from and to, in order.
	Blocks(ctx context.Context, from, to int64) ([]Block, error)
	// BlockBefore is the block before id, or Block{} if id is the first one.
	BlockBefore(ctx context.Context, id int64) (Block, error)
	// RecordsOf are the records of the block, in seq order.
	RecordsOf(ctx context.Context, blockID int64) ([]Record, error)
}

// ErrChainBroken names the block where the chain stops adding up: a record altered, inserted or deleted, a
// block changed or deleted, or an anchor that does not match or is missing.
type ErrChainBroken struct {
	BlockID int64
	Reason  string
}

func (e ErrChainBroken) Error() string {
	return fmt.Sprintf("the audit chain breaks at block %d: %s", e.BlockID, e.Reason)
}

// Seal seals up to maxRecords records with no block into one block, within the store's transaction: hash =
// SHA-256(previous block hash ‖ records by seq). First it anchors the last existing block (re-anchoring the
// same hash does nothing): if its anchor failed, it is completed here, and if it fails again nothing is
// sealed on top, so only the last block can be left unanchored. It returns the new block, or Block{} if
// there were no records to seal; the caller anchors it after committing (see audit.Service.Seal).
func Seal(ctx context.Context, store SealStore, anchor AnchorStore, maxRecords int) (Block, error) {
	records, err := store.Unsealed(ctx, maxRecords)
	if err != nil {
		return Block{}, err
	}
	last, err := store.LastBlock(ctx)
	if err != nil {
		return Block{}, err
	}
	if last.ID != 0 {
		if err := anchor.Put(ctx, last.ID, last.Hash); err != nil {
			return Block{}, err
		}
	}
	if len(records) == 0 {
		return Block{}, nil
	}
	sealed := Block{PrevHash: last.Hash, Records: len(records)}
	if sealed.Hash, err = blockHash(last.Hash, records); err != nil {
		return Block{}, err
	}
	if sealed.ID, err = store.InsertBlock(ctx, sealed.PrevHash, sealed.Hash, sealed.Records); err != nil {
		return Block{}, err
	}
	seqs := make([]int64, len(records))
	for i, r := range records {
		seqs[i] = r.Seq
	}
	return sealed, store.AssignBlock(ctx, sealed.ID, seqs)
}

// Verify walks the ids from from to to (up to the last block in the database) in order: each block must
// start at the previous block's hash (including the one before from), yield its hash from its records and
// match its anchor, and an id with no block must have no anchor (a deleted block). The first one that fails
// is ErrChainBroken; a range with nothing sealed is ErrNoBlocks. Blocks deleted after the last one in the
// database are not seen (Q-88).
func Verify(ctx context.Context, store VerifyStore, anchor AnchorStore, from, to int64) error {
	last, err := store.LastBlock(ctx)
	if err != nil {
		return err
	}
	to = min(to, last.ID) // with from > to there are no ids to check: ErrNoBlocks
	blocks, err := store.Blocks(ctx, from, to)
	if err != nil {
		return err
	}
	prev, err := store.BlockBefore(ctx, from)
	if err != nil {
		return err
	}
	if err := walk(ctx, store, anchor, prev, blocks, from, to); err != nil {
		return err
	}
	if len(blocks) == 0 {
		return ErrNoBlocks
	}
	return nil
}

// walk goes through the ids from from to to in order: one with a block (blocks, in order) is verified
// against the previous block's hash, starting with prev; one without must have no anchor.
func walk(ctx context.Context, store VerifyStore, anchor AnchorStore, prev Block, blocks []Block, from, to int64) error {
	for id := from; id <= to; id++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(blocks) == 0 || blocks[0].ID != id {
			if err := notAnchored(ctx, anchor, id); err != nil {
				return err
			}
			continue
		}
		if err := verifyBlock(ctx, store, anchor, prev.Hash, blocks[0]); err != nil {
			return err
		}
		prev, blocks = blocks[0], blocks[1:]
	}
	return nil
}

// notAnchored is nil if the id has no anchor: a seal that did not commit. With an anchor, the block was
// deleted.
func notAnchored(ctx context.Context, anchor AnchorStore, id int64) error {
	_, err := anchor.Get(ctx, id)
	switch {
	case err == nil:
		return ErrChainBroken{BlockID: id, Reason: "it is anchored but no longer in the database"}
	case errors.Is(err, ErrNotAnchored):
		return nil
	default:
		return err
	}
}

func verifyBlock(ctx context.Context, store VerifyStore, anchor AnchorStore, prevHash [32]byte, b Block) error {
	if b.PrevHash != prevHash {
		return ErrChainBroken{BlockID: b.ID, Reason: "it does not start at the previous block's hash"}
	}
	records, err := store.RecordsOf(ctx, b.ID)
	if err != nil {
		return err
	}
	hash, err := blockHash(b.PrevHash, records)
	if err != nil {
		return err
	}
	if len(records) != b.Records || hash != b.Hash {
		return ErrChainBroken{BlockID: b.ID, Reason: "its records do not yield its hash"}
	}
	anchored, err := anchor.Get(ctx, b.ID)
	if errors.Is(err, ErrNotAnchored) {
		return ErrChainBroken{BlockID: b.ID, Reason: "it has no anchor"}
	}
	if err != nil {
		return err
	}
	if anchored != b.Hash {
		return ErrChainBroken{BlockID: b.ID, Reason: "its hash is not the anchored one"}
	}
	return nil
}

// canonicalRecord fixes the order and form of each field in the hash: changing one changes the hash of
// every block sealed with it.
type canonicalRecord struct {
	Seq           int64           `json:"seq"`
	RecordedAt    string          `json:"recorded_at"`
	Actor         string          `json:"actor"`
	Role          string          `json:"role"`
	Channel       string          `json:"channel"`
	IP            string          `json:"ip"`
	CorrelationID string          `json:"correlation_id"`
	Command       string          `json:"command"`
	Targets       []string        `json:"targets"`
	Result        string          `json:"result"`
	Before        json.RawMessage `json:"before"`
	After         json.RawMessage `json:"after"`
}

// blockHash is SHA-256(prev ‖ one JSON per record ending in '\n'): compact JSON, without escaping <, > or &
// (so any language can recompute it), recorded_at in UTC with microseconds (the database precision) and
// before/after as jsonb returns them, compacted.
func blockHash(prev [32]byte, records []Record) ([32]byte, error) {
	var buf bytes.Buffer
	buf.Write(prev[:])
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, r := range records {
		err := enc.Encode(canonicalRecord{
			Seq: r.Seq, RecordedAt: r.RecordedAt.UTC().Format("2006-01-02T15:04:05.000000Z"), Actor: r.Actor, Role: r.Role,
			Channel: r.Channel, IP: r.IP, CorrelationID: r.CorrelationID, Command: r.Command, Targets: r.Targets,
			Result: r.Result, Before: r.Before, After: r.After,
		})
		if err != nil {
			return [32]byte{}, fmt.Errorf("encode record %d: %w", r.Seq, err)
		}
	}
	return sha256.Sum256(buf.Bytes()), nil
}
