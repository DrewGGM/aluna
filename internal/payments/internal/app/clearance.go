package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/payments/internal/domain"
)

// Why an order that waited for compliance is rejected: ISO 20022 ExternalStatusReason1Code, because here the
// entity refuses its own customer's payment as a bank would (unlike RAIL_REFUSED, where nobody refused it).
const (
	ReasonScreeningConfirmed = "RR04" // regulatory reason: compliance confirmed the match
	ReasonInsufficientFunds  = "AM04" // the account no longer has the amount and its charges
	ReasonAccountBlocked     = "AC06" // the account is no longer active
	ReasonLimitExceeded      = "AM02" // the amount would take the account over a limit of its product
)

// Clearance is what applying compliance's decision on an order needs, within its transaction.
type Clearance interface {
	// LockOrder locks and returns order id, or ErrOrderNotFound.
	LockOrder(ctx context.Context, id ids.ID) (Order, error)
	DepositAccount(ctx context.Context, entity, id ids.ID) (deposits.Account, error)
	funds
	// Store stores where o stands, with its hold if it has one, and publishes it; a held order is queued to be
	// sent.
	Store(ctx context.Context, o Order) error
}

// ResolveScreening applies compliance's decision (FR-42) on order id of entity, pending_screening: a confirmed
// match rejects it (RR04); a cleared one holds its amount and charges, its GMF as it is cleared and its fees as
// policy says of those it was quoted (ADR-0016), and the order is held and queued to be sent. An order that is no
// longer pending_screening expired meanwhile (ExpirePending) and is left as it is. Another entity's order is
// ErrOrderNotFound. If the funds cannot be held it returns why (HoldRefusal names the reason): the caller undoes
// what tx did since and calls RejectPending.
func ResolveScreening(ctx context.Context, p Clearance, entity, id ids.ID, cleared bool, policy Policy) error {
	o, err := lockOrderOf(ctx, p, entity, id)
	if err != nil || o.Status != domain.StatusPendingScreening {
		return err
	}
	if !cleared {
		return rejectPending(ctx, p, o, ReasonScreeningConfirmed)
	}
	a, err := p.DepositAccount(ctx, entity, o.AccountID)
	if err != nil {
		return err
	}
	if err := hold(ctx, p, a, &o, ids.ID{}, policy); err != nil {
		return err
	}
	return p.Store(ctx, o)
}

// HoldRefusal is the reason to reject an order for if err is why its funds could not be held, or "" if it is
// not: no funds (AM04), an account no longer active (AC06) or over a limit of its product (AM02).
func HoldRefusal(err error) string {
	switch {
	case errors.Is(err, ledger.ErrInsufficientFunds):
		return ReasonInsufficientFunds
	case errors.Is(err, deposits.ErrAccountNotActive):
		return ReasonAccountBlocked
	case errors.Is(err, deposits.ErrLimitExceeded):
		return ReasonLimitExceeded
	}
	return ""
}

// RejectPending rejects order id of entity for reason if it is still pending_screening, and leaves it as it is if it
// expired meanwhile. It locks the order again: rolling back to the savepoint taken before ResolveScreening released
// the lock that call took, and the expiry may have taken the order in between.
func RejectPending(ctx context.Context, p Clearance, entity, id ids.ID, reason string) error {
	o, err := lockOrderOf(ctx, p, entity, id)
	if err != nil || o.Status != domain.StatusPendingScreening {
		return err
	}
	return rejectPending(ctx, p, o, reason)
}

// lockOrderOf locks order id of entity; another entity's is ErrOrderNotFound.
func lockOrderOf(ctx context.Context, p Clearance, entity, id ids.ID) (Order, error) {
	o, err := p.LockOrder(ctx, id)
	if err != nil {
		return Order{}, err
	}
	if o.LegalEntityID != entity {
		return Order{}, fmt.Errorf("%w: %s", ErrOrderNotFound, id)
	}
	return o, nil
}

func rejectPending(ctx context.Context, p Clearance, o Order, reason string) error {
	if err := o.Reject(reason); err != nil {
		return err
	}
	return p.Store(ctx, o)
}
