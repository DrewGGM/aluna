// Package domain holds the pricing rules: which fees an operation pays.
package domain

import "github.com/DrewGGM/aluna/internal/money"

// Operation is an operation the products charge for.
type Operation string

// Operations with fees.
const (
	Withdrawal Operation = "withdrawal"
	Transfer   Operation = "transfer"
)

// FeeRule is a fee of a product: AmountUnits on each Operation, unless the rule named ExemptRule holds (none
// if empty).
type FeeRule struct {
	Code        string
	Operation   Operation
	AmountUnits int64
	ExemptRule  string
}

// Fee is a fee an operation pays.
type Fee struct {
	Code   string
	Amount money.Amount
}

// FeesFor are the fees operation op pays (FR-28), in the order of the product's rules and in currency c: those
// of the operation whose exemption does not hold, as exempt decides it for a rule's name. A fee without an
// exemption rule is always paid.
func FeesFor(rules []FeeRule, op Operation, c money.Currency, exempt func(rule string) bool) []Fee {
	var fees []Fee
	for _, r := range rules {
		if r.Operation != op || (r.ExemptRule != "" && exempt(r.ExemptRule)) {
			continue
		}
		fees = append(fees, Fee{Code: r.Code, Amount: money.New(r.AmountUnits, c)})
	}
	return fees
}

// Total is what fees add up to, in currency c.
func Total(fees []Fee, c money.Currency) (money.Amount, error) {
	total := money.New(0, c)
	for _, f := range fees {
		var err error
		if total, err = total.Add(f.Amount); err != nil {
			return money.Amount{}, err
		}
	}
	return total, nil
}
