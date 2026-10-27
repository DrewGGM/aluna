package app

import (
	"context"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger/internal/domain"
)

// AsyncCursor es hasta dónde agregó el agregador: el último entry sumado y la transacción de base que lo
// asentó (posted_xact). Avanza en orden de (Xact, EntryID).
type AsyncCursor struct {
	Xact    uint64
	EntryID int64
}

// AggregationStore es la persistencia del agregador de saldos async.
type AggregationStore interface {
	// LockAsyncCursor bloquea y lee el cursor: un solo agregador avanza a la vez, así que nadie más
	// escribe el saldo de una cuenta async (Post no lo toca y un hold no sale de una async).
	LockAsyncCursor(ctx context.Context) (AsyncCursor, error)
	// AsyncEntriesAfter lee hasta limit entries de cuentas async posteriores a c, en orden, solo de
	// transacciones de base que ya no están en curso, y el cursor del último leído.
	AsyncEntriesAfter(ctx context.Context, c AsyncCursor, limit int) ([]domain.EntryInput, AsyncCursor, error)
	AddToBalance(ctx context.Context, accountID ids.ID, d BalanceDelta) error
	MoveAsyncCursor(ctx context.Context, c AsyncCursor) error
}

// AggregateAsyncBalances suma a los saldos de las cuentas async un lote de hasta limit entries que aún
// no agregó (FR-9) y mueve el cursor en la misma transacción: si algo falla, ni los saldos ni el cursor
// cambian, y agregar otra vez no duplica. Devuelve cuántos entries agregó.
func AggregateAsyncBalances(ctx context.Context, store AggregationStore, limit int) (int, error) {
	from, err := store.LockAsyncCursor(ctx)
	if err != nil {
		return 0, err
	}
	entries, to, err := store.AsyncEntriesAfter(ctx, from, limit)
	if err != nil || len(entries) == 0 {
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
