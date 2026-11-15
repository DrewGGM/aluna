package app

import (
	"testing"

	"github.com/DrewGGM/aluna/internal/ids"
)

// TestTheReversalOfATransactionIsFoundByItsReference (Q-97 a): a transaction is reversed when the reversal that
// names it holds its reference; one that holds the reference without reversing it is an error, not a reversal.
func TestTheReversalOfATransactionIsFoundByItsReference(t *testing.T) {
	entity, original, other := ids.New(), ids.New(), ids.New()
	store := newFakeStore()

	if _, found, err := ReversalOf(t.Context(), store, entity, original); err != nil || found {
		t.Fatalf("ReversalOf an unreversed transaction = %v, %v; want not found", found, err)
	}
	reversal := Transaction{TransactionRecord: TransactionRecord{ID: ids.New(), ReversesID: original}}
	store.posted[ref{entity, reversalSource, original.String()}] = reversal
	if got, found, err := ReversalOf(t.Context(), store, entity, original); err != nil || !found || got.ID != reversal.ID {
		t.Fatalf("ReversalOf = %+v, %v, %v; want its reversal", got, found, err)
	}
	store.posted[ref{entity, reversalSource, other.String()}] = Transaction{TransactionRecord: TransactionRecord{ID: ids.New(), ReversesID: original}}
	if _, found, err := ReversalOf(t.Context(), store, entity, other); err == nil || found {
		t.Fatalf("ReversalOf a reference held by another reversal = %v, %v; want an error", found, err)
	}
}
