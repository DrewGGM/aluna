// Package postingrules has the posting rules of deposits (ADR-0011): pure functions from a movement to its
// ledger entries, with the version the transaction records. The sign is the ledger's (debit positive); the
// customer's account is a liability, so money into it is a credit to it and money out of it a debit.
package postingrules

import (
	"errors"
	"fmt"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
)

// RuleVersion is the version of these rules, recorded as the transaction's rule_version.
const RuleVersion = "deposits/v1"

// ErrInvalidMovement means a movement the rules cannot post: an amount that is not positive or has no
// currency, or a missing or repeated account.
var ErrInvalidMovement = errors.New("invalid deposit movement")

// Movement is money between a customer's account and another account. A zero amount (an exempt fee, an
// exempt GMF, nothing to capitalize) is not a movement: the caller skips it.
type Movement struct {
	Amount  money.Amount // positive, in the accounts' currency
	Account ids.ID       // the customer's ledger account; for a transfer, the one the money leaves
	Counter ids.ID       // the other side of each rule
}

// CashDeposit is cash into the customer's account: Counter is the cash (or vault) account it comes from.
func CashDeposit(in Movement) ([]ledger.EntryInput, error) { return into(in) }

// Withdrawal is cash out of the customer's account to Counter, the cash account.
func Withdrawal(in Movement) ([]ledger.EntryInput, error) { return outOf(in) }

// InternalTransfer moves money from the customer's account to Counter, another deposit account of the
// entity.
func InternalTransfer(in Movement) ([]ledger.EntryInput, error) { return outOf(in) }

// InterestCapitalization posts the interest due into the customer's account; Counter is the product's
// interest expense account (FR-26).
func InterestCapitalization(in Movement) ([]ledger.EntryInput, error) { return into(in) }

// FeeCharge charges a fee to the customer's account; Counter is the product's fee income account.
func FeeCharge(in Movement) ([]ledger.EntryInput, error) { return outOf(in) }

// GMFWithholding withholds the GMF from the customer's account; Counter is the GMF payable account
// (FR-29).
func GMFWithholding(in Movement) ([]ledger.EntryInput, error) { return outOf(in) }

// into is the money of in going into the customer's account: Counter debited, Account credited.
func into(in Movement) ([]ledger.EntryInput, error) { return entries(in, in.Counter, in.Account) }

// outOf is the money of in leaving the customer's account: Account debited, Counter credited.
func outOf(in Movement) ([]ledger.EntryInput, error) { return entries(in, in.Account, in.Counter) }

func entries(in Movement, debited, credited ids.ID) ([]ledger.EntryInput, error) {
	switch {
	case in.Amount.Sign() <= 0 || in.Amount.Currency() == (money.Currency{}):
		return nil, fmt.Errorf("%w: amount of %d units of %q", ErrInvalidMovement, in.Amount.Units(), in.Amount.Currency().Code())
	case in.Account == (ids.ID{}) || in.Counter == (ids.ID{}):
		return nil, fmt.Errorf("%w: missing account", ErrInvalidMovement)
	case in.Account == in.Counter:
		return nil, fmt.Errorf("%w: account %s on both sides", ErrInvalidMovement, in.Account)
	}
	// the amount is positive, so its opposite fits in int64
	credit := money.New(-in.Amount.Units(), in.Amount.Currency())
	return []ledger.EntryInput{{AccountID: debited, Amount: in.Amount}, {AccountID: credited, Amount: credit}}, nil
}
