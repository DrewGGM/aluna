// Package domain is the deposits domain: deposit accounts and their life cycle (FR-22).
package domain

import (
	"errors"
	"fmt"
	"slices"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
)

// Status is the status of a deposit account.
type Status string

// Statuses of a deposit account.
const (
	Pending Status = "pending"
	Active  Status = "active"
	Frozen  Status = "frozen"
	Dormant Status = "dormant"
	Closed  Status = "closed"
)

var (
	// ErrInvalidTransition means the account cannot go from its status to the requested one.
	ErrInvalidTransition = errors.New("invalid deposit account status transition")
	// ErrNotEmpty means an account with a balance cannot close.
	ErrNotEmpty = errors.New("the account has a balance")
	// ErrPendingHolds means an account with pending holds cannot close.
	ErrPendingHolds = errors.New("the account has pending holds")
)

// transitions are the diagram of docs/architecture.md (FR-22): a pending account is activated when its
// KYC is approved or closed if rejected; an active one is frozen and unfrozen by an order, goes dormant
// without customer activity and comes back with it; an active or dormant one closes. Closed is final.
var transitions = map[Status][]Status{
	Pending: {Active, Closed},
	Active:  {Frozen, Dormant, Closed},
	Frozen:  {Active},
	Dormant: {Active, Closed},
}

// Account is a deposit account: the party's contract with a version of a product, held in one ledger
// account (liability) of the product's currency.
type Account struct {
	ID, LegalEntityID, PartyID, ProductVersionID, LedgerAccountID ids.ID
	Currency                                                      money.Currency
	Status                                                        Status
	OpenedOn, LastCustomerActivityOn                              calendar.Date
}

// CheckTransition reports whether an account may go from from to to, or ErrInvalidTransition.
func CheckTransition(from, to Status) error {
	if slices.Contains(transitions[from], to) {
		return nil
	}
	return fmt.Errorf("%w: %s to %s", ErrInvalidTransition, from, to)
}

// OpeningStatus is the status an account opens with, once its party's KYC is one the product allows: active
// if the KYC is approved, pending otherwise (a product such as a simplified deposit may allow a pending KYC;
// the account is activated when it is approved).
func OpeningStatus(kycApproved bool) Status {
	if kycApproved {
		return Active
	}
	return Pending
}

// CheckClosable reports whether an account with ledger balance b may close: with no pending hold on either
// side and a posted balance of 0, or ErrPendingHolds or ErrNotEmpty. The holds go first: an account that
// cannot go below 0 only holds funds it has, so a pending hold comes with a balance, and the hold is what has
// to be resolved first.
func CheckClosable(b ledger.Balance) error {
	if b.PendingDebits != 0 || b.PendingCredits != 0 {
		return fmt.Errorf("%w: %d pending debits, %d pending credits", ErrPendingHolds, b.PendingDebits, b.PendingCredits)
	}
	if b.Posted != 0 {
		return fmt.Errorf("%w: posted %d", ErrNotEmpty, b.Posted)
	}
	return nil
}
