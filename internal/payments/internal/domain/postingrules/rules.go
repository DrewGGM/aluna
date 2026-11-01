// Package postingrules has the posting rules of payments (ADR-0011): pure functions from a payment's charges
// to their ledger entries, with the version the transaction records. The sign is the ledger's (debit
// positive); the customer's account is a liability, so money out of it is a debit to it. The payment's amount
// is posted by the capture of its hold, with the ledger's own rule.
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

// ErrInvalidCharge means a charge the rules cannot post: an amount that is not positive or has no currency,
// or a missing or repeated account.
var ErrInvalidCharge = errors.New("invalid payment charge")

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
		switch {
		case c.Amount.Sign() <= 0 || c.Amount.Currency() == (money.Currency{}):
			return nil, fmt.Errorf("%w: amount of %d units of %q", ErrInvalidCharge, c.Amount.Units(), c.Amount.Currency().Code())
		case account == (ids.ID{}) || c.Counter == (ids.ID{}):
			return nil, fmt.Errorf("%w: missing account", ErrInvalidCharge)
		case account == c.Counter:
			return nil, fmt.Errorf("%w: account %s on both sides", ErrInvalidCharge, account)
		}
		// the amount is positive, so its opposite fits in int64
		entries = append(entries, ledger.EntryInput{AccountID: account, Amount: c.Amount},
			ledger.EntryInput{AccountID: c.Counter, Amount: money.New(-c.Amount.Units(), c.Amount.Currency())})
	}
	return entries, nil
}
