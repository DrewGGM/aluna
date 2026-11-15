package app

import (
	"context"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger/internal/domain"
)

// AsyncCursor is how far the aggregator has aggregated: the last summed entry and the database
// transaction that posted it (posted_xact) or, once every async entry below the horizon is summed, the xid just
// below it and MaxInt64, past all of its entries. It advances in (Xact, EntryID) order.
type AsyncCursor struct {
	Xact    uint64
	EntryID int64
}

// Before reports whether c is before d in (Xact, EntryID) order.
func (c AsyncCursor) Before(d AsyncCursor) bool {
	return c.Xact < d.Xact || c.Xact == d.Xact && c.EntryID < d.EntryID
}

// AggregationStore is the persistence of the async balance aggregator.
type AggregationStore interface {
	// LockAsyncCursor locks and reads the cursor: only one aggregator advances at a time, so nobody else
	// writes an async account's balance (Post does not touch it and a hold does not leave an async one).
	LockAsyncCursor(ctx context.Context) (AsyncCursor, error)
	// AsyncEntriesAfter reads up to limit async-account entries after c, in order, only from database
	// transactions older than the oldest one in progress, and the cursor to move to: the last one read or, if it
	// read fewer than limit, the end of every transaction older than that one (Q-62).
	AsyncEntriesAfter(ctx context.Context, c AsyncCursor, limit int) ([]domain.EntryInput, AsyncCursor, error)
	AddToBalance(ctx context.Context, accountID ids.ID, d BalanceDelta) error
	MoveAsyncCursor(ctx context.Context, c AsyncCursor) error
}

// AggregateAsyncBalances adds to the async accounts' balances a batch of up to limit entries not yet
// aggregated (FR-9) and moves the cursor in the same transaction: if something fails, neither the
// balances nor the cursor change, and aggregating again does not duplicate. The cursor moves wherever the store
// says, past the last entry or past the transactions with no async entry after it, and stays when that is not
// ahead. It returns how many entries it aggregated.
func AggregateAsyncBalances(ctx context.Context, store AggregationStore, limit int) (int, error) {
	from, err := store.LockAsyncCursor(ctx)
	if err != nil {
		return 0, err
	}
	entries, to, err := store.AsyncEntriesAfter(ctx, from, limit)
	if err != nil || !from.Before(to) {
		return 0, err
	}
	deltas, err := netByAccount(entries)
	if err != nil {
		return 0, err
	}
	for id, d := range deltas {
		if err := store.AddToBalance(ctx, id, BalanceDelta{Posted: d.Units()}); err != nil {
			return 0, err
		}
	}
	if err := store.MoveAsyncCursor(ctx, to); err != nil {
		return 0, err
	}
	return len(entries), nil
}
