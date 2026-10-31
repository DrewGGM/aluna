package domain

import (
	"errors"
	"fmt"
	"slices"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
)

var (
	// ErrTooFewEntries means a transaction has fewer than two entries.
	ErrTooFewEntries = errors.New("a transaction needs at least two entries")
	// ErrZeroAmount means an entry has a zero amount.
	ErrZeroAmount = errors.New("an entry cannot have a zero amount")
	// ErrUnbalanced means the entries do not sum to zero in some currency.
	ErrUnbalanced = errors.New("the transaction does not sum to zero")
)

// EntryInput is one line of a transaction. Amount is signed: debit positive, credit negative.
type EntryInput struct {
	AccountID ids.ID
	Amount    money.Amount
}

// NewTransactionInput is the data to build a transaction.
type NewTransactionInput struct {
	ID      ids.ID
	Entries []EntryInput
}

// Transaction is a transaction that already satisfies double entry: at least two entries, none of them
// zero, and a zero sum in each currency.
type Transaction struct {
	id      ids.ID
	entries []EntryInput
}

// NewTransaction validates double entry and returns the transaction; money is neither created nor
// destroyed.
func NewTransaction(in NewTransactionInput) (Transaction, error) {
	if len(in.Entries) < 2 {
		return Transaction{}, fmt.Errorf("%w: has %d", ErrTooFewEntries, len(in.Entries))
	}
	sums := map[money.Currency]money.Amount{}
	var order []money.Currency // so the error always names the same currency
	for i, e := range in.Entries {
		if e.Amount.IsZero() {
			return Transaction{}, fmt.Errorf("%w: entry %d", ErrZeroAmount, i)
		}
		cur := e.Amount.Currency()
		sum, ok := sums[cur]
		if !ok {
			sum = money.New(0, cur) // the zero Amount has no currency and Add rejects it
			order = append(order, cur)
		}
		next, err := sum.Add(e.Amount)
		if err != nil {
			return Transaction{}, fmt.Errorf("entry %d: %w", i, err)
		}
		sums[cur] = next
	}
	for _, cur := range order {
		if sum := sums[cur]; !sum.IsZero() {
			return Transaction{}, fmt.Errorf("%w: %s sums to %d", ErrUnbalanced, cur.Code(), sum.Units())
		}
	}
	return Transaction{id: in.ID, entries: slices.Clone(in.Entries)}, nil
}

// ID is the transaction's identifier.
func (t Transaction) ID() ids.ID { return t.id }

// Entries returns a copy of the entries: the validated transaction cannot be altered from outside.
func (t Transaction) Entries() []EntryInput { return slices.Clone(t.entries) }
