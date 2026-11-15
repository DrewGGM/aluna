package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func goldenRecords() []Record {
	return []Record{
		{
			Seq: 7, RecordedAt: time.Date(2026, time.October, 27, 10, 4, 5, 123456000, time.FixedZone("COT", -5*3600)),
			Actor: "operator", Role: "treasurer", Channel: "api", IP: "10.0.0.7", CorrelationID: "c-1",
			Command: "POST /v1/transactions", Targets: []string{"t-1", "t-2"}, Result: "ok",
			After: json.RawMessage(`{"id": "t-1"}`),
		},
		{
			Seq: 9, RecordedAt: time.Date(2026, time.October, 27, 15, 4, 6, 0, time.UTC),
			Actor: "Zoë <ops> & co", Role: "api", Channel: "api", IP: "::1", CorrelationID: "c-2",
			Command: "POST /v1/holds/{id}/release", Targets: []string{}, Result: "hold_not_pending",
			Before: json.RawMessage(`{"status": "released"}`),
		},
	}
}

// TestTheBlockHashIsPinned (ADR-0012): the evidence format does not change. A block's hash is SHA-256 of the
// previous hash followed by one compact JSON per record, ending in '\n', with all its fields in this order,
// recorded_at in UTC with microseconds, before/after compacted and without escaping <, > or & (any language
// recomputes it the same way).
func TestTheBlockHashIsPinned(t *testing.T) {
	prev := sha256.Sum256([]byte("previous block"))
	lines := `{"seq":7,"recorded_at":"2026-10-27T15:04:05.123456Z","actor":"operator","role":"treasurer","channel":"api",` +
		`"ip":"10.0.0.7","correlation_id":"c-1","command":"POST /v1/transactions","targets":["t-1","t-2"],"result":"ok",` +
		`"before":null,"after":{"id":"t-1"}}` + "\n" +
		`{"seq":9,"recorded_at":"2026-10-27T15:04:06.000000Z","actor":"Zoë <ops> & co","role":"api","channel":"api",` +
		`"ip":"::1","correlation_id":"c-2","command":"POST /v1/holds/{id}/release","targets":[],"result":"hold_not_pending",` +
		`"before":{"status":"released"},"after":null}` + "\n"
	want := sha256.Sum256(append(prev[:], lines...))

	got, err := blockHash(prev, goldenRecords())

	if err != nil || got != want {
		t.Fatalf("hash = %x (%v), want %x", got, err, want)
	}
}

// lastBlocks is a VerifyStore whose last block is the next of ids at each read, the last one from then on; it reads
// nothing else.
type lastBlocks struct {
	VerifyStore
	ids []int64
}

func (l *lastBlocks) LastBlock(context.Context) (Block, error) {
	id := l.ids[0]
	if len(l.ids) > 1 {
		l.ids = l.ids[1:]
	}
	return Block{ID: id}, nil
}

// anchors is an AnchorStore in memory; Get of the id in failing is err.
type anchors struct {
	AnchorStore
	hashes  map[int64][32]byte
	failing int64
	err     error
}

func (a anchors) Get(_ context.Context, id int64) ([32]byte, error) {
	if id == a.failing {
		return [32]byte{}, a.err
	}
	hash, ok := a.hashes[id]
	if !ok {
		return [32]byte{}, ErrNotAnchored
	}
	return hash, nil
}

// TestAnAnchorAheadOfTheDatabaseIsCaught (Q-138 f): with the database's last block 10, an anchor that also holds
// block 11, or 43 (the end of the window), is ErrAnchorAhead; one that holds up to 10, or block 44, past the window,
// is fine.
func TestAnAnchorAheadOfTheDatabaseIsCaught(t *testing.T) {
	for _, id := range []int64{11, 43} {
		err := CheckAnchor(t.Context(), &lastBlocks{ids: []int64{10}}, anchors{hashes: map[int64][32]byte{1: {}, 10: {}, id: {}}})
		if want := (ErrAnchorAhead{BlockID: id, LastBlock: 10}); !errors.Is(err, want) {
			t.Errorf("anchor with block %d: CheckAnchor = %v, want %v", id, err, want)
		}
	}
	for _, id := range []int64{10, 44} {
		if err := CheckAnchor(t.Context(), &lastBlocks{ids: []int64{10}}, anchors{hashes: map[int64][32]byte{1: {}, id: {}}}); err != nil {
			t.Errorf("anchor up to block %d: CheckAnchor = %v, want nil", id, err)
		}
	}
}

// TestABlockSealedDuringTheCheckIsNotAhead: another sealer that commits and anchors block 11 while the check reads
// is in the database when the check reads again, so it is not a restore.
func TestABlockSealedDuringTheCheckIsNotAhead(t *testing.T) {
	err := CheckAnchor(t.Context(), &lastBlocks{ids: []int64{10, 11}}, anchors{hashes: map[int64][32]byte{11: {}}})
	if err != nil {
		t.Fatalf("CheckAnchor = %v, want nil", err)
	}
}

// TestAnAnchorThatFailsIsNotAhead: an anchor that does not answer is its error, not ErrAnchorAhead.
func TestAnAnchorThatFailsIsNotAhead(t *testing.T) {
	unavailable := errors.New("anchor unavailable")
	err := CheckAnchor(t.Context(), &lastBlocks{ids: []int64{10}}, anchors{failing: 12, err: unavailable})
	if !errors.Is(err, unavailable) || errors.As(err, new(ErrAnchorAhead)) {
		t.Fatalf("CheckAnchor = %v, want %v", err, unavailable)
	}
}
