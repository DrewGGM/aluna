package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger/internal/domain"
)

// ErrAlreadyReversed means a transaction that already has its reversal: Reverse returns the existing
// reversal together with this error.
var ErrAlreadyReversed = errors.New("the transaction was already reversed")

// Reversal header.
const (
	reversalSource  = "ledger.reversal"
	reversalCode    = "reversal"
	reversalChannel = "ledger"
)

// ReverseInput asks to reverse transaction TransactionID of entity LegalEntityID.
type ReverseInput struct {
	LegalEntityID ids.ID
	TransactionID ids.ID
	Reason, Actor string
	BusinessDate  time.Time
}

// ReversalStore is the persistence of Reverse: Post's plus reading the reversed transaction.
type ReversalStore interface {
	PostingStore
	TransactionOf(ctx context.Context, entity, id ids.ID) (Transaction, error)
}

// Reverse posts the reversal of a transaction (FR-3): its entries with the opposite sign, with the
// original's value date and in's business date. It goes through Post, so it honors the limits: reversing
// an already spent deposit is ErrInsufficientFunds. If the transaction already has a reversal, it returns
// it with ErrAlreadyReversed and writes nothing; if it posts it, it emits transaction.reversed.
func Reverse(ctx context.Context, store ReversalStore, guard PeriodGuard, events Events, in ReverseInput) (Transaction, error) {
	if in.Reason == "" {
		return Transaction{}, fmt.Errorf("%w: missing reversal reason", ErrInvalidPosting)
	}
	original, err := store.TransactionOf(ctx, in.LegalEntityID, in.TransactionID)
	if err != nil {
		return Transaction{}, err
	}
	entries, err := domain.ReversalEntries(original.Entries)
	if err != nil {
		return Transaction{}, err
	}
	// The external reference is the reversed transaction: the uniqueness of (source, external_ref) allows
	// at most one reversal per transaction, also between two concurrent reversals.
	reversal, err := post(ctx, store, guard, PostInput{
		LegalEntityID: in.LegalEntityID, Code: reversalCode, ValueDate: original.ValueDate, BusinessDate: in.BusinessDate,
		Source: reversalSource, ExternalRef: original.ID.String(), Actor: in.Actor, Channel: reversalChannel,
		RuleVersion: domain.ReversalRuleVersion, ReversesID: original.ID, Metadata: map[string]string{"reason": in.Reason},
		Entries: entries,
	}, pendingRelease{})
	if err == nil {
		if err := events.Emit(ctx, transactionEvent(typeTransactionReversed, reversal)); err != nil {
			return Transaction{}, err
		}
		return reversal, nil
	}
	if !errors.Is(err, ErrDuplicateExternalRef) {
		return reversal, err
	}
	if reversal.ReversesID != original.ID { // the database does not stop direct SQL from taking the reference: fail closed
		return Transaction{}, fmt.Errorf("the reversal reference of %s is held by %s, which does not reverse it", original.ID, reversal.ID)
	}
	return reversal, fmt.Errorf("%w: %s is already reversed by %s", ErrAlreadyReversed, original.ID, reversal.ID)
}
