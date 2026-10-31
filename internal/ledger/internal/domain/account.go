// Package domain holds the pure ledger rules (ADR-0003): the balanced transaction and an account's
// available balance. It knows neither the database nor the time.
package domain

import (
	"errors"
	"fmt"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
)

var (
	// ErrInsufficientFunds means a movement leaves an account's available balance below its limit.
	ErrInsufficientFunds = errors.New("insufficient funds")
	// ErrUnknownKind means an account has an accounting kind outside the five defined ones.
	ErrUnknownKind = errors.New("unknown account kind")
)

// Kind is an account's accounting kind; it decides its normal side (debit or credit).
type Kind string

// Accounting kinds.
const (
	KindAsset     Kind = "asset"
	KindLiability Kind = "liability"
	KindEquity    Kind = "equity"
	KindIncome    Kind = "income"
	KindExpense   Kind = "expense"
)

// BalanceMode says how an account's materialized balance is maintained (ADR-0004).
type BalanceMode string

// Balance modes: sync is updated in the same transaction as the posting; async is aggregated by a
// separate process (highly contended internal accounts).
const (
	BalanceModeSync  BalanceMode = "sync"
	BalanceModeAsync BalanceMode = "async"
)

// Account is a ledger account. LowerLimit is in minor units and in natural terms: 0 allows no negative
// balance; -5_000_000 in COP allows a 50,000-peso overdraft.
type Account struct {
	ID            ids.ID
	LegalEntityID ids.ID
	Code          string
	Kind          Kind
	Currency      money.Currency
	Mode          BalanceMode
	LowerLimit    int64
}

// NaturalBalance converts the signed balance (debit +, credit −) to the balance on the account's normal
// side: assets and expenses are debit-normal; liabilities, equity and income are credit-normal. A
// 100,000-peso deposit leaves posted = -10_000_000 on the customer account and its natural balance is
// 10_000_000. On a credit account, posted = MinInt64 has no opposite in int64. An unknown kind is treated
// as credit-normal; that is why CheckAvailable, which decides whether money moves, rejects it first.
func NaturalBalance(k Kind, posted int64) int64 {
	if debit, _ := debitNormal(k); debit {
		return posted
	}
	return -posted
}

// debitNormal reports whether the kind is debit-normal; ok is false if the kind does not exist.
func debitNormal(k Kind) (debit, ok bool) {
	switch k {
	case KindAsset, KindExpense:
		return true, true
	case KindLiability, KindEquity, KindIncome:
		return false, true
	}
	return false, false
}

// CheckAvailable decides whether the account accepts delta, a transaction's net (signed) movement on it,
// given its posted balance and its pending holds (non-negative amounts). The available balance is the
// natural balance minus outgoing holds (pending debits on a credit account, pending credits on a debit
// one); incoming holds do not add. Only a movement that reduces the natural balance is rejected: an
// inflow to an account already below its limit improves it. Everything is computed with money: an
// overflow is an error and the movement does not go through.
func (a Account) CheckAvailable(posted, pendingDebits, pendingCredits, delta int64) error {
	debit, ok := debitNormal(a.Kind)
	if !ok { // without a normal side there is no telling which way money leaves: it does not move
		return fmt.Errorf("%w: account %s, kind %q", ErrUnknownKind, a.Code, a.Kind)
	}
	if reduces := (delta < 0) == debit && delta != 0; !reduces {
		return nil
	}
	available, err := a.availableAfter(debit, posted, pendingDebits, pendingCredits, delta)
	if err != nil {
		return fmt.Errorf("account %s: %w", a.Code, err)
	}
	if available.Units() < a.LowerLimit {
		return fmt.Errorf("%w: account %s, available %d, limit %d", ErrInsufficientFunds, a.Code, available.Units(), a.LowerLimit)
	}
	return nil
}

// availableAfter is only called with a movement that reduces the natural balance. On a credit account
// that is a debit (delta > 0), so posted + delta > MinInt64 and the natural balance always has an
// opposite.
func (a Account) availableAfter(debit bool, posted, pendingDebits, pendingCredits, delta int64) (money.Amount, error) {
	after, err := money.New(posted, a.Currency).Add(money.New(delta, a.Currency))
	if err != nil {
		return money.Amount{}, err
	}
	outgoing := pendingCredits
	if !debit {
		outgoing = pendingDebits
	}
	natural := money.New(NaturalBalance(a.Kind, after.Units()), a.Currency)
	return natural.Sub(money.New(outgoing, a.Currency))
}
