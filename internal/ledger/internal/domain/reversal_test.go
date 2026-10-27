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
		t.Fatalf("ReversalEntries = %+v, %v; se esperaba %+v", reversed, err, want)
	}
	if _, err := NewTransaction(NewTransactionInput{ID: ids.New(), Entries: reversed}); err != nil {
		t.Fatalf("la reversa de una transacción balanceada también lo está: %v", err)
	}
}

// TestReversalEntriesOfMinInt64Fail: -MinInt64 no cabe en int64; la reversa falla en vez de dar la vuelta.
func TestReversalEntriesOfMinInt64Fail(t *testing.T) {
	cop := currency(t, "COP")
	if _, err := ReversalEntries([]EntryInput{{AccountID: ids.New(), Amount: money.New(math.MinInt64, cop)}}); !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("= %v, se esperaba money.ErrOverflow", err)
	}
}
