package app

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger/internal/domain"
)

// ExpiryStore es la persistencia de la expiración de holds.
type ExpiryStore interface {
	Accounts(ctx context.Context, accountIDs []ids.ID) (map[ids.ID]domain.Account, error)
	LockBalances(ctx context.Context, accountIDs []ids.ID) (map[ids.ID]LockedBalance, error)
	AddToBalance(ctx context.Context, accountID ids.ID, d BalanceDelta) error
	CloseHold(ctx context.Context, h Hold, captureTxID ids.ID) error
	// LockDueHolds bloquea (FOR UPDATE SKIP LOCKED) hasta limit holds pending vencidos a now (expires_at
	// anterior a now, como en Hold.Expire), de cualquier entidad: los que otra transacción tiene
	// bloqueados, como un capture en curso, se saltan.
	LockDueHolds(ctx context.Context, now time.Time, limit int) ([]Hold, error)
}

// ExpireDueHolds expira un lote de hasta limit holds vencidos a now (FR-6): los marca expired y quita su
// importe del pendiente de la cuenta origen. Bloquea los holds antes que los saldos (ADR-0004), como el
// capture; un hold que el capture ya bloqueó no entra al lote, y si lo bloquea primero la expiración,
// el capture lo ve expirado. Emite hold.expired de cada uno y devuelve cuántos expiró.
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
		// el lote mezcla entidades: la del hold es la de su cuenta
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

// releasePending resta de cada cuenta lo que liberan sus holds, con una escritura por cuenta. Bloquea
// antes los saldos en orden de cuenta, como un posting, para tomar los locks en el mismo orden.
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
