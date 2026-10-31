package app

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger/internal/domain"
)

// ExpiryStore is the persistence of hold expiry.
type ExpiryStore interface {
	Accounts(ctx context.Context, accountIDs []ids.ID) (map[ids.ID]domain.Account, error)
	LockBalances(ctx context.Context, accountIDs []ids.ID) (map[ids.ID]LockedBalance, error)
	AddToBalance(ctx context.Context, accountID ids.ID, d BalanceDelta) error
	CloseHold(ctx context.Context, h Hold, captureTxID ids.ID) error
	// LockDueHolds locks (FOR UPDATE SKIP LOCKED) up to limit pending holds due at now (expires_at before
	// now, as in Hold.Expire), from any entity: those another transaction has locked, such as a capture
	// in progress, are skipped.
	LockDueHolds(ctx context.Context, now time.Time, limit int) ([]Hold, error)
}

// ExpireDueHolds expires a batch of up to limit holds due at now (FR-6): it marks them expired and
// removes their amount from the source account's pending balance. It locks the holds before the balances
// (ADR-0004), like the capture; a hold the capture already locked does not enter the batch, and if the
// expiry locks it first, the capture sees it expired. It emits hold.expired for each one and returns how
// many it expired.
func ExpireDueHolds(ctx context.Context, store ExpiryStore, events Events, now time.Time, limit int) (int, error) {
	due, err := store.LockDueHolds(ctx, now, limit)
	if err != nil || len(due) == 0 {
		return 0, err
	}
	released := make(map[ids.ID]BalanceDelta)
	for _, h := range due {
		released[h.AccountID()] = BalanceDelta{}
	}
	accounts, err := store.Accounts(ctx, slices.Collect(maps.Keys(released)))
	if err != nil {
		return 0, err
	}
	for _, h := range due {
		next, err := h.hold.Expire(now)
		if err != nil {
			return 0, err
		}
		expired := h.with(next)
		if err := store.CloseHold(ctx, expired, ids.ID{}); err != nil {
			return 0, err
		}
		// the batch mixes entities: the hold's entity is its account's
		if err := events.Emit(ctx, holdEvent(typeHoldExpired, accounts[h.AccountID()].LegalEntityID, expired, ids.ID{})); err != nil {
			return 0, err
		}
		debits, credits := pendingOf(outgoingOf(accounts[h.AccountID()], h.Amount()))
		d := released[h.AccountID()]
		d.PendingDebits, d.PendingCredits = d.PendingDebits-debits, d.PendingCredits-credits
		released[h.AccountID()] = d
	}
	if err := releasePending(ctx, store, released); err != nil {
		return 0, err
	}
	return len(due), nil
}

// releasePending subtracts from each account what its holds release, with one write per account. It
// first locks the balances in account order, like a posting, to take the locks in the same order.
func releasePending(ctx context.Context, store ExpiryStore, released map[ids.ID]BalanceDelta) error {
	if _, err := store.LockBalances(ctx, slices.Collect(maps.Keys(released))); err != nil {
		return err
	}
	for id, d := range released {
		if err := store.AddToBalance(ctx, id, d); err != nil {
			return err
		}
	}
	return nil
}
