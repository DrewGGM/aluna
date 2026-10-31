//go:build integration

package audit_test

import (
	"context"
	"errors"
	"math"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/audit"
	"github.com/DrewGGM/aluna/internal/audit/internal/adapters/anchorfs"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// sealer is an audit service that seals with the anchor in a temporary directory.
func sealer(t *testing.T) *audit.Service {
	t.Helper()
	return audit.NewSealing(dirAnchor(t))
}

func dirAnchor(t *testing.T) *anchorfs.Dir {
	t.Helper()
	anchor, err := anchorfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return anchor
}

func seal(t *testing.T, s *audit.Service, maxRecords int) (int64, error) {
	t.Helper()
	return s.Seal(t.Context(), postgres.NewUnitOfWork(testPool), maxRecords)
}

// sealAll seals everything with no block, in blocks of at most maxRecords, and returns the blocks.
func sealAll(t *testing.T, s *audit.Service, maxRecords int) []int64 {
	t.Helper()
	var blocks []int64
	for {
		id, err := seal(t, s, maxRecords)
		if err != nil {
			t.Fatal(err)
		}
		if id == 0 {
			return blocks
		}
		blocks = append(blocks, id)
	}
}

// sealedRecords stores n records and seals them with s; it returns their correlations and the blocks it
// sealed. The database is shared by the whole package: the blocks may also carry records from other tests,
// and each test verifies only its own, anchored in its directory. A test that alters a block does not get
// in the way of the next ones: Seal chains from the hash stored in the database.
func sealedRecords(t *testing.T, s *audit.Service, n int) ([]string, []int64) {
	t.Helper()
	correlations := make([]string, n)
	for i := range correlations {
		r := record()
		if err := save(t, r); err != nil {
			t.Fatal(err)
		}
		correlations[i] = r.CorrelationID
	}
	blocks := sealAll(t, s, 2)
	if len(blocks) == 0 {
		t.Fatal("no block was sealed")
	}
	return correlations, blocks
}

func blockOf(t *testing.T, correlation string) int64 {
	t.Helper()
	var block *int64
	if err := testPool.QueryRow(t.Context(), `SELECT block_id FROM audit.records WHERE correlation_id = $1`, correlation).Scan(&block); err != nil {
		t.Fatal(err)
	}
	if block == nil {
		t.Fatalf("record %s was not sealed", correlation)
	}
	return *block
}

// tamper runs stmts as the owner with the audit table triggers disabled: what a database administrator
// would do to alter the evidence.
func tamper(t *testing.T, stmts ...string) {
	t.Helper()
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		all := append([]string{"ALTER TABLE audit.records DISABLE TRIGGER USER", "ALTER TABLE audit.blocks DISABLE TRIGGER USER"}, stmts...)
		all = append(all, "ALTER TABLE audit.records ENABLE TRIGGER USER", "ALTER TABLE audit.blocks ENABLE TRIGGER USER")
		for _, stmt := range all {
			if _, err := tx.Exec(t.Context(), stmt); err != nil {
				return err
			}
		}
		return nil
	})
}

func expectBrokenAt(t *testing.T, s *audit.Service, from, to, block int64) {
	t.Helper()
	err := s.Verify(t.Context(), testPool, from, to)
	var broken audit.ErrChainBroken
	if !errors.As(err, &broken) || broken.BlockID != block {
		t.Fatalf("Verify(%d, %d) = %v, want the chain broken at block %d", from, to, err, block)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// TestSealedBlocksVerify (ADR-0012): sealing chains the records into blocks of at most maxRecords, every
// record ends up in a block and the chain verifies against its anchors.
func TestSealedBlocksVerify(t *testing.T) {
	s := sealer(t)
	correlations, blocks := sealedRecords(t, s, 3)

	for _, c := range correlations {
		blockOf(t, c)
	}
	if len(blocks) < 2 {
		t.Fatalf("3 records in blocks of 2 gave %d blocks", len(blocks))
	}
	if err := s.Verify(t.Context(), testPool, blocks[0], blocks[len(blocks)-1]); err != nil {
		t.Fatalf("Verify of an untouched chain = %v", err)
	}
	if id, err := seal(t, s, 2); err != nil || id != 0 {
		t.Fatalf("sealing with no pending records = %d, %v; want 0", id, err)
	}
}

// TestTamperingBreaksTheChain (AB-7): altering or deleting a sealed record, even as the owner and with no
// triggers, or changing the hash of its block or of the previous one (which is not in the verified range),
// makes Verify fail at that block.
func TestTamperingBreaksTheChain(t *testing.T) {
	cases := map[string]func(t *testing.T, correlation string, block int64){
		"altered record": func(t *testing.T, c string, _ int64) {
			tamper(t, `UPDATE audit.records SET result = 'fake-ok' WHERE correlation_id = '`+c+`'`)
		},
		"deleted record": func(t *testing.T, c string, _ int64) {
			tamper(t, `DELETE FROM audit.records WHERE correlation_id = '`+c+`'`)
		},
		"block with a different hash": func(t *testing.T, _ string, b int64) {
			tamper(t, `UPDATE audit.blocks SET hash = sha256(hash) WHERE id = `+itoa(b))
		},
		"previous block with a different hash": func(t *testing.T, _ string, b int64) {
			tamper(t, `UPDATE audit.blocks SET hash = sha256(hash) WHERE id = (SELECT max(id) FROM audit.blocks WHERE id < `+itoa(b)+`)`)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s := sealer(t)
			sealedRecords(t, s, 1) // a block before the one holding the test's record
			correlations, _ := sealedRecords(t, s, 1)
			block := blockOf(t, correlations[0])
			if err := s.Verify(t.Context(), testPool, block, block); err != nil {
				t.Fatalf("before tampering: %v", err)
			}

			change(t, correlations[0], block)

			expectBrokenAt(t, s, block, block, block)
		})
	}
}

// TestTruncatingAndResealingIsDetected: whoever deletes the last block, changes its records and lets them be
// sealed again does not fool Verify: the deleted block has an anchor and is no longer in the database.
func TestTruncatingAndResealingIsDetected(t *testing.T) {
	s := sealer(t)
	correlations, _ := sealedRecords(t, s, 1)
	block := blockOf(t, correlations[0])

	tamper(t,
		`UPDATE audit.records SET block_id = NULL, result = 'fake-ok' WHERE block_id = `+itoa(block),
		`DELETE FROM audit.blocks WHERE id = `+itoa(block))
	resealed := sealAll(t, s, 100)

	expectBrokenAt(t, s, block, resealed[len(resealed)-1], block)
}

// TestABlockWithoutItsAnchorBreaksTheChain: verifying against an anchor that lacks the block, or that has a
// different hash, breaks the chain at that block.
func TestABlockWithoutItsAnchorBreaksTheChain(t *testing.T) {
	correlations, _ := sealedRecords(t, sealer(t), 1)
	block := blockOf(t, correlations[0])
	wrong := dirAnchor(t)
	if err := wrong.Put(t.Context(), block, [32]byte{1}); err != nil {
		t.Fatal(err)
	}

	expectBrokenAt(t, audit.NewSealing(dirAnchor(t)), block, block, block)
	expectBrokenAt(t, audit.NewSealing(wrong), block, block, block)
}

// TestAnEmptyRangeIsNotIntact: a range with no blocks or anchors is not taken as verified.
func TestAnEmptyRangeIsNotIntact(t *testing.T) {
	far := lastBlock(t) + 1000

	if err := sealer(t).Verify(t.Context(), testPool, far, far+10); !errors.Is(err, audit.ErrNoBlocks) {
		t.Fatalf("Verify of an empty range = %v, want ErrNoBlocks", err)
	}
}

// flakyAnchor is an anchor whose first failures Puts of blocks after after fail, like storage that is down
// for a while.
type flakyAnchor struct {
	*anchorfs.Dir
	after    int64
	failures int
}

var errAnchorDown = errors.New("the anchor does not respond")

func (a *flakyAnchor) Put(ctx context.Context, id int64, hash [32]byte) error {
	if id > a.after && a.failures > 0 {
		a.failures--
		return errAnchorDown
	}
	return a.Dir.Put(ctx, id, hash)
}

// TestAFailedAnchorIsRetried: if anchoring fails after the block commits, the block stays sealed without an
// anchor; while the anchor is still down nothing is sealed on top, and when it comes back it is completed
// and the whole chain verifies.
func TestAFailedAnchorIsRetried(t *testing.T) {
	sealAll(t, sealer(t), 100) // what other tests left pending is not part of this one
	s := audit.NewSealing(&flakyAnchor{Dir: dirAnchor(t), after: lastBlock(t), failures: 2})
	first, second := record(), record()
	if err := save(t, first); err != nil {
		t.Fatal(err)
	}

	if _, err := seal(t, s, 100); !errors.Is(err, errAnchorDown) {
		t.Fatalf("first seal = %v, want the anchor failure", err)
	}
	block := blockOf(t, first.CorrelationID) // committed, with no anchor
	if err := save(t, second); err != nil {
		t.Fatal(err)
	}
	if _, err := seal(t, s, 100); !errors.Is(err, errAnchorDown) {
		t.Fatalf("seal with the anchor still down = %v, want the failure", err)
	}
	if next := lastBlock(t); next != block {
		t.Fatalf("block %d was sealed on top of %d, which has no anchor", next, block)
	}
	if _, err := seal(t, s, 100); err != nil {
		t.Fatal(err)
	}

	if err := s.Verify(t.Context(), testPool, block, blockOf(t, second.CorrelationID)); err != nil {
		t.Fatalf("Verify after the anchor came back = %v", err)
	}
}

// TestTheFirstBrokenBlockIsReported: with one block altered and a later one deleted, Verify names the first.
func TestTheFirstBrokenBlockIsReported(t *testing.T) {
	s := sealer(t)
	altered, _ := sealedRecords(t, s, 1)
	deleted, _ := sealedRecords(t, s, 1)
	first, second := blockOf(t, altered[0]), blockOf(t, deleted[0])
	tamper(t,
		`UPDATE audit.records SET result = 'fake-ok' WHERE correlation_id = '`+altered[0]+`'`,
		`UPDATE audit.records SET block_id = NULL WHERE block_id = `+itoa(second),
		`DELETE FROM audit.blocks WHERE id = `+itoa(second))

	expectBrokenAt(t, s, first, second, first)
}

// TestTheAppRoleCannotRewriteTheChain: aluna_app can only set block_id, and only to a block created in its
// same transaction; a sealed record does not change block, not even as the owner; two blocks cannot hang
// from the same previous one.
func TestTheAppRoleCannotRewriteTheChain(t *testing.T) {
	correlations, _ := sealedRecords(t, sealer(t), 1)
	c, block := correlations[0], blockOf(t, correlations[0])
	pending := record()
	if err := save(t, pending); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		stmt, state string
		owner       bool
	}{
		"change the result":               {`UPDATE audit.records SET result = 'other' WHERE correlation_id = '` + c + `'`, "42501", false},
		"remove its block":                {`UPDATE audit.records SET block_id = NULL WHERE correlation_id = '` + c + `'`, "2F003", false},
		"put a pending one in an old one": {`UPDATE audit.records SET block_id = ` + itoa(block) + ` WHERE correlation_id = '` + pending.CorrelationID + `'`, "2F003", false},
		"rewrite the block as the owner":  {`UPDATE audit.records SET block_id = block_id WHERE correlation_id = '` + c + `'`, "2F003", true},
		"fork the chain":                  {`INSERT INTO audit.blocks (prev_hash, hash, records) SELECT prev_hash, hash, 1 FROM audit.blocks WHERE id = ` + itoa(block), "23505", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var state string
			if tc.owner {
				state = ownerSQLState(t, tc.stmt)
			} else {
				_, err := testPool.Exec(t.Context(), tc.stmt)
				state = sqlState(err)
			}
			if state != tc.state {
				t.Fatalf("%s = SQLSTATE %q, want %q", tc.stmt, state, tc.state)
			}
		})
	}
}

// TestConcurrentSealersDoNotForkTheChain: several sealers at once leave a single chain: each block starts at
// the previous block's hash (the one that loses the race fails on the prev_hash uniqueness).
func TestConcurrentSealersDoNotForkTheChain(t *testing.T) {
	s := sealer(t)
	for range 20 {
		if err := save(t, record()); err != nil {
			t.Fatal(err)
		}
	}
	first := lastBlock(t) + 1
	var wg sync.WaitGroup
	sealed := make([]int64, 4)
	for i := range sealed {
		wg.Go(func() {
			id, err := seal(t, s, 3)
			if err != nil && sqlState(err) != "23505" { // losing the prev_hash race is the only expected failure
				t.Errorf("sealer %d: %v", i, err)
			}
			sealed[i] = id
		})
	}
	wg.Wait()
	if slices.Max(sealed) == 0 {
		t.Fatal("no concurrent sealer sealed a block")
	}
	sealAll(t, s, 3)

	if err := s.Verify(t.Context(), testPool, first, lastBlock(t)); err != nil {
		t.Fatalf("Verify after sealing in parallel = %v", err)
	}
}

func lastBlock(t *testing.T) int64 {
	t.Helper()
	var id int64
	if err := testPool.QueryRow(t.Context(), `SELECT coalesce(max(id), 0) FROM audit.blocks`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

var errUndo = errors.New("rolled back on purpose")

// intoFreshBlockAsOwner creates a block as the owner in a transaction, tries to move the correlation's
// record into it with the extra SQL assignment extraSet, returns the SQLSTATE and rolls everything back:
// the chain does not change.
func intoFreshBlockAsOwner(t *testing.T, correlation, extraSet string) string {
	t.Helper()
	var state string
	err := pgx.BeginFunc(t.Context(), testPool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), "SET LOCAL ROLE NONE"); err != nil {
			return err
		}
		var block int64
		if err := tx.QueryRow(t.Context(), `INSERT INTO audit.blocks (prev_hash, hash, records)
			VALUES (sha256(convert_to(gen_random_uuid()::text, 'UTF8')), sha256('x'::bytea), 1) RETURNING id`).Scan(&block); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), `UPDATE audit.records SET block_id = $1`+extraSet+` WHERE correlation_id = $2`, block, correlation)
		state = sqlState(err)
		return errUndo
	})
	if !errors.Is(err, errUndo) && state == "" {
		t.Fatal(err)
	}
	return state
}

// TestSealingInItsOwnTransactionSetsOnlyBlockID: in the transaction that creates a block, a pending record
// can move into it; an already sealed one cannot, and nobody, not even the owner, changes another field
// while sealing.
func TestSealingInItsOwnTransactionSetsOnlyBlockID(t *testing.T) {
	correlations, _ := sealedRecords(t, sealer(t), 1)
	pending := record()
	if err := save(t, pending); err != nil {
		t.Fatal(err)
	}

	if state := intoFreshBlockAsOwner(t, pending.CorrelationID, ""); state != "" {
		t.Fatalf("sealing a pending record into a new block = SQLSTATE %q, want success", state)
	}
	if state := intoFreshBlockAsOwner(t, pending.CorrelationID, ", result = 'other'"); state != "2F003" {
		t.Errorf("sealing and changing the result = SQLSTATE %q, want the trigger (2F003)", state)
	}
	if state := intoFreshBlockAsOwner(t, correlations[0], ""); state != "2F003" {
		t.Errorf("moving a sealed record to another block = SQLSTATE %q, want the trigger (2F003)", state)
	}
}

// TestAWideRangeStopsAtTheLastBlock: verifying "everything" with a huge to (up to the int64 maximum) checks
// up to the last block in the database and finishes, without an anchor lookup for each id in the range.
func TestAWideRangeStopsAtTheLastBlock(t *testing.T) {
	s := sealer(t)
	correlations, _ := sealedRecords(t, s, 1)
	block := blockOf(t, correlations[0])

	if err := s.Verify(t.Context(), testPool, block, math.MaxInt64); err != nil {
		t.Fatalf("Verify up to the maximum = %v", err)
	}
}
