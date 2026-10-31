package domain

import (
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
)

func TestReversalEntriesInvertTheSignInOrder(t *testing.T) {
	cop := currency(t, "COP")
	a, b := ids.New(), ids.New()
	entries := []EntryInput{{AccountID: a, Amount: money.New(100, cop)}, {AccountID: b, Amount: money.New(-100, cop)}}

	reversed, err := ReversalEntries(entries)

	want := []EntryInput{{AccountID: a, Amount: money.New(-100, cop)}, {AccountID: b, Amount: money.New(100, cop)}}
	if err != nil || !slices.Equal(reversed, want) {
		t.Fatalf("ReversalEntries = %+v, %v; want %+v", reversed, err, want)
	}
	if _, err := NewTransaction(NewTransactionInput{ID: ids.New(), Entries: reversed}); err != nil {
		t.Fatalf("the reversal of a balanced transaction is balanced too: %v", err)
	}
}

// TestReversalEntriesOfMinInt64Fail: -MinInt64 does not fit in int64; the reversal fails instead of
// wrapping around.
func TestReversalEntriesOfMinInt64Fail(t *testing.T) {
	cop := currency(t, "COP")
	if _, err := ReversalEntries([]EntryInput{{AccountID: ids.New(), Amount: money.New(math.MinInt64, cop)}}); !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("= %v, want money.ErrOverflow", err)
	}
}
