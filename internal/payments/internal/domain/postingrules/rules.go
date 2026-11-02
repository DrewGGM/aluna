// Package postingrules has the posting rules of payments (ADR-0011): pure functions from a payment's charges,
// or from a transfer received, to their ledger entries, with the version the transaction records. The sign is
// the ledger's (debit positive); the customer's account is a liability, so money out of it is a debit to it and
// money into it a credit. An outgoing payment's amount is posted by the capture of its hold, with the ledger's
// own rule.
package postingrules

import (
	"errors"
	"fmt"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
)

// RuleVersion is the version of these rules, recorded as the transaction's rule_version.
const RuleVersion = "payments/v1"

// Transaction codes of a transfer received, one per rule.
const (
	CodeInboundCredit   = "payments.inbound_credit"
	CodeInboundSuspense = "payments.inbound_suspense"
)

// ErrInvalidMovement means a charge or a transfer the rules cannot post: an amount that is not positive or has
// no currency, or a missing or repeated account.
var ErrInvalidMovement = errors.New("invalid payment movement")

// Charge is money a payment takes from the customer's account besides its amount: a fee, to Counter, the
// product's fee income account, or the GMF, to Counter, its GMF payable account (FR-28, FR-29).
type Charge struct {
	Amount  money.Amount // positive, in the account's currency
	Counter ids.ID
}

// Charges are the entries of a payment's charges from the customer's ledger account: each one debits account
// and credits its counter, in the order given. No charges are no entries.
func Charges(account ids.ID, charges []Charge) ([]ledger.EntryInput, error) {
	entries := make([]ledger.EntryInput, 0, 2*len(charges))
	for _, c := range charges {
		moved, err := move(c.Amount, account, c.Counter)
		if err != nil {
			return nil, err
		}
		entries = append(entries, moved...)
	}
	return entries, nil
}

// InboundCredit is a transfer received for an active account of the entity (FR-32), posted as
// CodeInboundCredit: it debits the rail's clearing account and credits customer, the account's ledger account.
func InboundCredit(amount money.Amount, clearing, customer ids.ID) ([]ledger.EntryInput, error) {
	return move(amount, clearing, customer)
}

// InboundSuspense is a transfer received for no account the entity can credit (FR-32), posted as
// CodeInboundSuspense: it debits the rail's clearing account and credits its suspense account until someone
// resolves it.
func InboundSuspense(amount money.Amount, clearing, suspense ids.ID) ([]ledger.EntryInput, error) {
	return move(amount, clearing, suspense)
}

// move is amount out of debited and into credited.
func move(amount money.Amount, debited, credited ids.ID) ([]ledger.EntryInput, error) {
	switch {
	case amount.Sign() <= 0 || amount.Currency() == (money.Currency{}):
		return nil, fmt.Errorf("%w: amount of %d units of %q", ErrInvalidMovement, amount.Units(), amount.Currency().Code())
	case debited == (ids.ID{}) || credited == (ids.ID{}):
		return nil, fmt.Errorf("%w: missing account", ErrInvalidMovement)
	case debited == credited:
		return nil, fmt.Errorf("%w: account %s on both sides", ErrInvalidMovement, debited)
	}
	// the amount is positive, so its opposite fits in int64
	return []ledger.EntryInput{{AccountID: debited, Amount: amount}, {AccountID: credited, Amount: money.New(-amount.Units(), amount.Currency())}}, nil
}
