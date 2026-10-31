// Package money is the money kernel (ADR-0005): integer amounts in a currency's minor units, with
// arithmetic that fails on overflow or mixed currencies instead of giving a wrong result.
package money

import (
	"cmp"
	"errors"
	"fmt"
	"math"
)

var (
	// ErrCurrencyMismatch means an operation between amounts in different currencies.
	ErrCurrencyMismatch = errors.New("currency mismatch")
	// ErrOverflow means the result does not fit in int64.
	ErrOverflow = errors.New("amount overflow")
)

// Amount is an amount in its currency's minor units: 100,000 COP is New(10_000_000, cop).
type Amount struct {
	units int64
	cur   Currency
}

// New creates an amount of units minor units of c.
func New(units int64, c Currency) Amount { return Amount{units: units, cur: c} }

// Units is the amount in minor units.
func (a Amount) Units() int64 { return a.units }

// Currency is the amount's currency.
func (a Amount) Currency() Currency { return a.cur }

// IsZero reports whether the amount is zero.
func (a Amount) IsZero() bool { return a.units == 0 }

// Sign returns -1, 0 or 1 according to the amount's sign.
func (a Amount) Sign() int { return cmp.Compare(a.units, 0) }

// Add adds b to a. It fails with ErrCurrencyMismatch if the currencies differ or the currency is
// missing, and with ErrOverflow if the result does not fit in int64; it never wraps around.
func (a Amount) Add(b Amount) (Amount, error) {
	if err := a.sameCurrency(b, "+"); err != nil {
		return Amount{}, err
	}
	sum := a.units + b.units
	if (b.units > 0 && sum < a.units) || (b.units < 0 && sum > a.units) {
		return Amount{}, fmt.Errorf("%w: %d + %d", ErrOverflow, a.units, b.units)
	}
	return New(sum, a.cur), nil
}

// Sub subtracts b from a, with the same errors as Add. It is not computed as a + (-b): -MinInt64 does
// not exist, but -1 - MinInt64 = MaxInt64 is a valid result.
func (a Amount) Sub(b Amount) (Amount, error) {
	if err := a.sameCurrency(b, "-"); err != nil {
		return Amount{}, err
	}
	diff := a.units - b.units
	if (b.units > 0 && diff > a.units) || (b.units < 0 && diff < a.units) {
		return Amount{}, fmt.Errorf("%w: %d - %d", ErrOverflow, a.units, b.units)
	}
	return New(diff, a.cur), nil
}

// Neg flips the amount's sign. MinInt64 has no opposite in int64.
func (a Amount) Neg() (Amount, error) {
	if a.units == math.MinInt64 {
		return Amount{}, fmt.Errorf("%w: -(%d)", ErrOverflow, a.units)
	}
	return New(-a.units, a.cur), nil
}

// sameCurrency also rejects the amount with no currency: Amount{} is what a discarded error returns and
// it must not be able to accumulate silently.
func (a Amount) sameCurrency(b Amount, op string) error {
	if a.cur != b.cur || a.cur.code == "" {
		return fmt.Errorf("%w: %q %s %q", ErrCurrencyMismatch, a.cur.code, op, b.cur.code)
	}
	return nil
}
