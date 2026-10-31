// Package audit is the command audit log (FR-40, ADR-0012): who asked for what, from where and what came of
// it, stored in the same transaction as the command's effect.
package audit

import (
	"context"
	"errors"
	"fmt"

	"github.com/DrewGGM/aluna/internal/audit/internal/adapters/anchorfs"
	pg "github.com/DrewGGM/aluna/internal/audit/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/audit/internal/app"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// ErrPersonalData means a Before or an After has a personal data key: the log stores a summary of the
// change, never data about people (Constitution VII).
var ErrPersonalData = errors.New("the audit log does not store personal data")

// personalKeys are the keys that mark personal data in a Before or an After, at any depth. They are
// compared as is: the summaries come from the API JSON, in snake_case.
var personalKeys = map[string]bool{"name": true, "document_number": true, "email": true, "phone": true}

// Record is an audit record. Command is the operation (the API route), Targets the IDs it touched, Result
// how it ended and Before/After the summary of the state.
type Record struct {
	Actor, Role, Channel, IP string
	CorrelationID            string
	Command                  string
	Targets                  []string
	Result                   string
	Before, After            map[string]any
}

// AnchorStore stores the hash of each sealed block outside the database (ADR-0012): WORM in production, a
// directory in development (anchorfs).
type AnchorStore = app.AnchorStore

// ErrChainBroken names the block where the audit chain stops adding up.
type ErrChainBroken = app.ErrChainBroken

// ErrNoBlocks means a Verify range with no sealed blocks: there is nothing to verify.
var ErrNoBlocks = app.ErrNoBlocks

var errNoAnchor = errors.New("the audit service has no anchor: it only records")

// Service stores audit records and, with an anchor, seals and verifies them.
type Service struct {
	anchor AnchorStore
}

// New creates the service that only records (the api): Seal and Verify fail without an anchor.
func New() *Service { return &Service{} }

// NewSealing creates the service that also seals and verifies against anchor.
func NewSealing(anchor AnchorStore) *Service { return &Service{anchor: anchor} }

// WithFileAnchor is NewSealing with the anchor in directory dir, which must exist; in production the
// anchor is WORM (Q-6).
func WithFileAnchor(dir string) (*Service, error) {
	anchor, err := anchorfs.New(dir)
	if err != nil {
		return nil, err
	}
	return NewSealing(anchor), nil
}

// Seal seals up to maxRecords committed records with no block into one block, in its own transaction, and
// anchors its hash after the commit (ADR-0012): an anchor always belongs to a committed block. If anchoring
// fails, the block stays unanchored and the next Seal completes it before sealing another. It returns the
// block id, or 0 if there was nothing to seal.
func (s *Service) Seal(ctx context.Context, uow *postgres.UnitOfWork, maxRecords int) (int64, error) {
	if s.anchor == nil {
		return 0, errNoAnchor
	}
	var sealed app.Block
	err := uow.Do(ctx, func(tx postgres.Tx) error {
		var err error
		sealed, err = app.Seal(ctx, pg.NewStore(tx), s.anchor, maxRecords)
		return err
	})
	if err != nil || sealed.ID == 0 {
		return 0, err
	}
	if err := s.anchor.Put(ctx, sealed.ID, sealed.Hash); err != nil {
		return 0, err
	}
	return sealed.ID, nil
}

// Verify recomputes the chain of blocks from to to and compares it with their anchors: a record altered,
// inserted or deleted, or a block changed or deleted within the range, is ErrChainBroken with the first
// block that does not add up; a range with no blocks is ErrNoBlocks. q can be the pool or a Tx.
func (s *Service) Verify(ctx context.Context, q postgres.Querier, from, to int64) error {
	if s.anchor == nil {
		return errNoAnchor
	}
	return app.Verify(ctx, pg.NewStore(q), s.anchor, from, to)
}

// Record stores r within tx, the command's transaction: if the command rolls back, so does its record. A
// Before or an After with personal data is ErrPersonalData and is not stored.
func (s *Service) Record(ctx context.Context, tx postgres.Tx, r Record) error {
	if err := rejectPersonalData("before", r.Before); err != nil {
		return err
	}
	if err := rejectPersonalData("after", r.After); err != nil {
		return err
	}
	return pg.Insert(ctx, tx, pg.Row{
		Actor: r.Actor, Role: r.Role, Channel: r.Channel, IP: r.IP, CorrelationID: r.CorrelationID, Command: r.Command,
		Targets: r.Targets, Result: r.Result, Before: r.Before, After: r.After,
	})
}

// rejectPersonalData is ErrPersonalData if v (what decoding JSON yields: maps, lists and values) has a
// personal data key at any depth; path says where, for the error.
func rejectPersonalData(path string, v any) error {
	switch v := v.(type) {
	case map[string]any:
		for k, inner := range v {
			if personalKeys[k] {
				return fmt.Errorf("%w: %s.%s", ErrPersonalData, path, k)
			}
			if err := rejectPersonalData(path+"."+k, inner); err != nil {
				return err
			}
		}
	case []any:
		for i, inner := range v {
			if err := rejectPersonalData(fmt.Sprintf("%s[%d]", path, i), inner); err != nil {
				return err
			}
		}
	}
	return nil
}
