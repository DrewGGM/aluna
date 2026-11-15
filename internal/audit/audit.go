// Package audit is the command audit log (FR-40, ADR-0012): who asked for what, from where and what came of
// it, stored in the same transaction as the command's effect.
package audit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/DrewGGM/aluna/internal/audit/internal/adapters/anchorfs"
	"github.com/DrewGGM/aluna/internal/audit/internal/adapters/anchors3"
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

// ErrAnchorAhead names an anchored block after the database's last one: a restore the anchor did not follow.
type ErrAnchorAhead = app.ErrAnchorAhead

var errNoAnchor = errors.New("the audit service has no anchor: it only records")

// Service stores audit records and, with an anchor, seals and verifies them. checked is whether CheckAnchor has
// passed in this process: until it does, Seal checks first.
type Service struct {
	anchor  AnchorStore
	checked atomic.Bool
}

// New creates the service that only records (the api): Seal and Verify fail without an anchor.
func New() *Service { return &Service{} }

// NewSealing creates the service that also seals and verifies against anchor.
func NewSealing(anchor AnchorStore) *Service { return &Service{anchor: anchor} }

// WithFileAnchor is NewSealing with the anchor in directory dir, which must exist: one read-only file per block,
// for development, or for a WORM volume that keeps hard links (ADR-0017).
func WithFileAnchor(dir string) (*Service, error) {
	anchor, err := anchorfs.New(dir)
	if err != nil {
		return nil, err
	}
	return NewSealing(anchor), nil
}

// S3Credentials are the keys that sign the requests to the anchor bucket.
type S3Credentials = anchors3.Credentials

// WithS3Anchor is NewSealing with the anchor in the S3 bucket with Object Lock at bucketURL (ADR-0017), under key
// prefix, in region, signed with creds through client: each block's anchor is created once and retained in
// compliance mode for retain, and one whose key has another version or a deletion does not read.
func WithS3Anchor(bucketURL, prefix, region string, creds S3Credentials, retain time.Duration, client *http.Client) (*Service, error) {
	anchor, err := anchors3.New(bucketURL, prefix, region, creds, retain, client)
	if err != nil {
		return nil, err
	}
	return NewSealing(anchor), nil
}

// Seal seals up to maxRecords committed records with no block into one block, in its own transaction, and
// anchors its hash after the commit (ADR-0012): an anchor always belongs to a committed block. If anchoring
// fails, the block stays unanchored and the next Seal completes it before sealing another. Until CheckAnchor has
// passed in this process, Seal checks the anchor first, and seals nothing on one ahead of the database
// (ErrAnchorAhead) or one it cannot check. It returns the block id, or 0 if there was nothing to seal.
func (s *Service) Seal(ctx context.Context, uow *postgres.UnitOfWork, maxRecords int) (int64, error) {
	if s.anchor == nil {
		return 0, errNoAnchor
	}
	if !s.checked.Load() {
		if err := s.CheckAnchor(ctx, uow); err != nil {
			return 0, err
		}
	}
	var sealed app.Block
	err := asSealer(ctx, uow, func(tx postgres.Tx) error {
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
// block that does not add up; a range with no blocks is ErrNoBlocks. The whole range is read in one transaction of
// uow, as the sealer.
func (s *Service) Verify(ctx context.Context, uow *postgres.UnitOfWork, from, to int64) error {
	if s.anchor == nil {
		return errNoAnchor
	}
	return asSealer(ctx, uow, func(tx postgres.Tx) error { return app.Verify(ctx, pg.NewStore(tx), s.anchor, from, to) })
}

// CheckAnchor is ErrAnchorAhead if the anchor holds a block after the database's last one, as after a restore to an
// earlier point without a new anchor generation: Seal does not seal on top of it. Once it passes, Seal stops
// checking. It reads in a transaction of uow, as the sealer.
func (s *Service) CheckAnchor(ctx context.Context, uow *postgres.UnitOfWork) error {
	if s.anchor == nil {
		return errNoAnchor
	}
	err := asSealer(ctx, uow, func(tx postgres.Tx) error { return app.CheckAnchor(ctx, pg.NewStore(tx), s.anchor) })
	if err != nil {
		return err
	}
	s.checked.Store(true)
	return nil
}

// sealerRole is the database role that reads and seals the audit log (Q-89, migration 0049).
const sealerRole = "aluna_sealer"

// asSealer runs fn in a transaction of uow as sealerRole; the user of the process must be a member of it.
func asSealer(ctx context.Context, uow *postgres.UnitOfWork, fn func(tx postgres.Tx) error) error {
	return uow.Do(ctx, func(tx postgres.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+sealerRole); err != nil {
			return fmt.Errorf("set role %s (the user of the process must be granted it): %w", sealerRole, err)
		}
		return fn(tx)
	})
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
