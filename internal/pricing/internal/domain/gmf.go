package domain

import (
	"fmt"

	"github.com/DrewGGM/aluna/internal/money"
)

// The GMF's terms (FR-29): it is levied in pesos at 4 per thousand, rounded to the peso, and a party's debits
// of a month are exempt up to 350 UVT.
const (
	gmfCurrency = "COP"
	gmfRate     = "0.004"
	exemptUVT   = "350"
)

// GMF is the GMF of a debit (FR-29): Exempt is the part of the debit the exemption covers and Tax, 4 per
// thousand of the rest.
type GMF struct {
	Exempt, Tax money.Amount
}

// Levied reports whether debits in currency c pay GMF: only those in COP do.
func Levied(c money.Currency) bool { return c.Code() == gmfCurrency }

// ExemptionCap is 350 UVT in COP, truncated to the minor unit: the most of a party's debits a month exempts.
func ExemptionCap(uvt money.Precise) (money.Amount, error) {
	c, err := money.CurrencyOf(gmfCurrency)
	if err != nil {
		return money.Amount{}, err
	}
	times, err := money.ParsePrecise(exemptUVT)
	if err != nil {
		return money.Amount{}, err
	}
	pesos, err := uvt.Mul(times)
	if err != nil {
		return money.Amount{}, fmt.Errorf("GMF exemption of a UVT of %s: %w", uvt, err)
	}
	limit, _, err := pesos.ToAmount(c, money.Down)
	return limit, err
}

// GMFOf is the GMF of debit, of which remaining can still be exempt (zero for an account without the
// exemption): the exemption covers what it can and the rest pays 4 per thousand, rounded half up to the peso.
// Only debits in COP pay it.
func GMFOf(debit, remaining money.Amount) (GMF, error) {
	c := debit.Currency()
	if !Levied(c) {
		return GMF{Exempt: money.New(0, c), Tax: money.New(0, c)}, nil
	}
	exempt := money.New(min(debit.Units(), remaining.Units()), c)
	taxed, err := debit.Sub(exempt)
	if err != nil {
		return GMF{}, err
	}
	tax, err := taxOf(taxed)
	if err != nil {
		return GMF{}, err
	}
	return GMF{Exempt: exempt, Tax: tax}, nil
}

// taxOf is 4 per thousand of taxed, rounded half up to the peso.
func taxOf(taxed money.Amount) (money.Amount, error) {
	rate, err := money.ParsePrecise(gmfRate)
	if err != nil {
		return money.Amount{}, err
	}
	peso, err := money.ParsePrecise("1")
	if err != nil {
		return money.Amount{}, err
	}
	tax, err := taxed.Precise().Mul(rate)
	if err != nil {
		return money.Amount{}, err
	}
	if tax, err = tax.Quo(peso, 0, money.HalfUp); err != nil {
		return money.Amount{}, err
	}
	amount, _, err := tax.ToAmount(taxed.Currency(), money.HalfUp)
	return amount, err
}
