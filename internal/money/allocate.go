package money

import (
	"errors"
	"fmt"
	"math/big"
	"slices"
)

// ErrInvalidWeights means allocation weights that are empty, negative or add up to zero.
var ErrInvalidWeights = errors.New("invalid allocation weights")

// Allocate splits a into parts proportional to weights using the largest remainder method: each part
// gets the floor of its exact quota and the leftover units go one by one to the parts with the largest
// remainder; on a tie, to the first. It is deterministic, the parts add up to exactly a and none is one
// unit or more away from its exact quota. A negative amount is split as its absolute value, with the
// sign. Like Add, it rejects the amount with no currency.
func (a Amount) Allocate(weights []int64) ([]Amount, error) {
	if a.cur.code == "" {
		return nil, fmt.Errorf("%w: allocation of an amount with no currency", ErrUnknownCurrency)
	}
	total, err := totalWeight(weights)
	if err != nil {
		return nil, err
	}
	whole := new(big.Int).Abs(big.NewInt(a.units))
	shares := make([]*big.Int, len(weights))
	remainders := make([]*big.Int, len(weights))
	left := new(big.Int).Set(whole)
	for i, w := range weights {
		quota := new(big.Int).Mul(whole, big.NewInt(w))
		shares[i], remainders[i] = quota.QuoRem(quota, total, new(big.Int))
		left.Sub(left, shares[i])
	}
	// left < len(weights): it is the sum of the fractional parts.
	order := make([]int, len(weights))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(i, j int) int { return remainders[j].Cmp(remainders[i]) })
	for _, i := range order[:left.Int64()] {
		shares[i].Add(shares[i], big.NewInt(1))
	}
	negative := a.units < 0
	parts := make([]Amount, len(weights))
	for i, s := range shares {
		if negative {
			s.Neg(s)
		}
		parts[i] = New(s.Int64(), a.cur)
	}
	return parts, nil
}

func totalWeight(weights []int64) (*big.Int, error) {
	if len(weights) == 0 {
		return nil, fmt.Errorf("%w: no weights", ErrInvalidWeights)
	}
	total := new(big.Int)
	for _, w := range weights {
		if w < 0 {
			return nil, fmt.Errorf("%w: negative weight %d", ErrInvalidWeights, w)
		}
		total.Add(total, big.NewInt(w))
	}
	if total.Sign() == 0 {
		return nil, fmt.Errorf("%w: weights add up to zero", ErrInvalidWeights)
	}
	return total, nil
}
