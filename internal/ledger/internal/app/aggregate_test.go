package app

import (
	"context"
	"testing"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger/internal/domain"
	"github.com/DrewGGM/aluna/internal/money"
)

// aggregation is an AggregationStore in memory: its batch and the cursor it answers with, and what it was told.
type aggregation struct {
	cursor, to AsyncCursor
	batch      []domain.EntryInput
	added      map[ids.ID]int64
	moved      []AsyncCursor
}

func (a *aggregation) LockAsyncCursor(context.Context) (AsyncCursor, error) { return a.cursor, nil }

func (a *aggregation) AsyncEntriesAfter(context.Context, AsyncCursor, int) ([]domain.EntryInput, AsyncCursor, error) {
	return a.batch, a.to, nil
}

func (a *aggregation) AddToBalance(_ context.Context, id ids.ID, d BalanceDelta) error {
	a.added[id] += d.Posted
	return nil
}

func (a *aggregation) MoveAsyncCursor(_ context.Context, c AsyncCursor) error {
	a.moved = append(a.moved, c)
	return nil
}

// TestTheCursorMovesWhereTheStoreSays (Q-62): a batch adds its entries by account and moves the cursor; an empty
// one moves it past the transactions with no async entry when the store says it is ahead, and leaves it when not.
func TestTheCursorMovesWhereTheStoreSays(t *testing.T) {
	cop, err := money.CurrencyOf("COP")
	if err != nil {
		t.Fatal(err)
	}
	account, at := ids.New(), AsyncCursor{Xact: 10, EntryID: 7}
	for name, c := range map[string]struct {
		batch      []domain.EntryInput
		to         AsyncCursor
		n          int
		moved, sum int64
	}{
		"a batch":                     {[]domain.EntryInput{{AccountID: account, Amount: money.New(-300, cop)}, {AccountID: account, Amount: money.New(100, cop)}}, AsyncCursor{Xact: 12, EntryID: 9}, 2, 1, -200},
		"an empty batch ahead":        {nil, AsyncCursor{Xact: 20, EntryID: 1<<63 - 1}, 0, 1, 0},
		"an empty batch where it was": {nil, at, 0, 0, 0},
	} {
		store := &aggregation{cursor: at, to: c.to, batch: c.batch, added: map[ids.ID]int64{}}
		n, err := AggregateAsyncBalances(t.Context(), store, 100)
		if err != nil || n != c.n || int64(len(store.moved)) != c.moved || store.added[account] != c.sum {
			t.Errorf("%s: %d entries (%v), moved %v, added %d; want %d, %d moves, %d", name, n, err, store.moved, store.added[account], c.n, c.moved, c.sum)
		}
		if c.moved == 1 && store.moved[0] != c.to {
			t.Errorf("%s: moved to %v, want %v", name, store.moved[0], c.to)
		}
	}
}
