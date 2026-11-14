package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/payments/internal/domain"
	"github.com/DrewGGM/aluna/internal/payments/internal/domain/postingrules"
)

// ErrNotSuspended means an order that is not a transfer held in suspense: there is nothing to credit.
var ErrNotSuspended = errors.New("the order is not a transfer held in suspense")

// orderLocker locks orders.
type orderLocker interface {
	// LockOrder locks and returns order id, or ErrOrderNotFound.
	LockOrder(ctx context.Context, id ids.ID) (Order, error)
}

// Crediting is what crediting a transfer held in suspense needs, within its transaction.
type Crediting interface {
	orderLocker
	railCrediting
	// CountCredit keeps a credit of amount to a, already locked, within its product's limits and counts it.
	CountCredit(ctx context.Context, a deposits.Account, amount money.Amount) error
	// Settle posts entries, o's credit from suspense, made by actor, on the entity's business date, and stores and
	// publishes o.
	Settle(ctx context.Context, o Order, actor string, entries []ledger.EntryInput) error
}

// LockSuspended locks order id of entity if it is a transfer held in suspense: ErrOrderNotFound if the entity has
// no such order, ErrNotSuspended if it is any other.
func LockSuspended(ctx context.Context, p orderLocker, entity, id ids.ID) (Order, error) {
	o, err := p.LockOrder(ctx, id)
	if err != nil {
		return Order{}, err
	}
	switch {
	case o.LegalEntityID != entity:
		return Order{}, fmt.Errorf("%w: %s", ErrOrderNotFound, id)
	case o.Direction != domain.Incoming || o.Status != domain.StatusSuspended:
		return Order{}, fmt.Errorf("%w: order %s is %s %s", ErrNotSuspended, id, o.Direction, o.Status)
	}
	return o, nil
}

// CreditSuspended credits transfer id of entity, held in suspense, to deposit account account, which someone found
// it was for, by actor (FR-32): its amount moves from the rail's suspense account to the account, which must be
// active (deposits.ErrAccountNotActive), in the transfer's currency (domain.ErrInvalidOrder) and within its
// product's limits (deposits.ErrLimitExceeded), and the order settles for it. Everything is checked before the
// limits are counted.
func CreditSuspended(ctx context.Context, p Crediting, entity, id, account ids.ID, actor string) (Order, error) {
	o, err := LockSuspended(ctx, p, entity, id)
	if err != nil {
		return Order{}, err
	}
	a, err := p.LockActive(ctx, entity, account)
	if err != nil {
		return Order{}, err
	}
	_, suspense, err := p.RailAccounts(ctx, entity, o.Rail, o.Amount.Currency())
	if err != nil {
		return Order{}, err
	}
	if err := o.CreditFromSuspense(a.ID, a.Currency); err != nil {
		return Order{}, err
	}
	entries, err := postingrules.SuspenseCredit(o.Amount, suspense, a.LedgerAccountID)
	if err != nil {
		return Order{}, err
	}
	if err := p.CountCredit(ctx, a, o.Amount); err != nil {
		return Order{}, err
	}
	return o, p.Settle(ctx, o, actor, entries)
}
