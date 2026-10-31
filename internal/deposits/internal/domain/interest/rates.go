// Package interest has the formulas of deposit interest (FR-25): day count conventions and the
// conversion of the effective annual rate (EA), which is exponential and is never divided (SFC: an EA "is
// not divided by any denominator").
package interest

import (
	"errors"
	"fmt"

	"github.com/DrewGGM/aluna/internal/money"
)

var (
	// ErrInvalidBasis means a day basis that is not 365 or 360.
	ErrInvalidBasis = errors.New("day basis is not 365 or 360")
	// ErrInvalidPeriods means a number of periods per year that is not positive.
	ErrInvalidPeriods = errors.New("periods per year are not positive")
	// ErrInvalidRate means an EA of -100 % or less, which has no periodic equivalent.
	ErrInvalidRate = errors.New("EA is -100 % or less")
)

// exponentScale is the number of decimals of the exponent 1/n of a root: as many as the 34 significant
// digits of Pow.
const exponentScale = 34

// DailyRateFromEA is the daily rate equivalent to ea over basis days, 365 or 360: (1 + ea)^(1/basis) − 1.
// It fits ACT/365F and ACT/360, where every day weighs the same; under 30E/360 and ACT/ACT a day weighs
// what YearFraction says, and the rate of the day is rateForFraction.
func DailyRateFromEA(ea money.Precise, basis int) (money.Precise, error) {
	if basis != 365 && basis != 360 {
		return money.Precise{}, fmt.Errorf("%w: %d", ErrInvalidBasis, basis)
	}
	exponent, err := quotient(1, basis, exponentScale)
	if err != nil {
		return money.Precise{}, err
	}
	return rateForFraction(ea, exponent)
}

// PeriodicRateFromEA is the rate of each of periodsPerYear periods equivalent to ea:
// (1 + ea)^(1/periodsPerYear) − 1.
func PeriodicRateFromEA(ea money.Precise, periodsPerYear int) (money.Precise, error) {
	if periodsPerYear < 1 {
		return money.Precise{}, fmt.Errorf("%w: %d", ErrInvalidPeriods, periodsPerYear)
	}
	exponent, err := quotient(1, periodsPerYear, exponentScale)
	if err != nil {
		return money.Precise{}, err
	}
	return rateForFraction(ea, exponent)
}

// NominalFromEA is the nominal annual rate paid over periodsPerYear periods equivalent to ea:
// periodsPerYear × PeriodicRateFromEA(ea, periodsPerYear).
func NominalFromEA(ea money.Precise, periodsPerYear int) (money.Precise, error) {
	rate, err := PeriodicRateFromEA(ea, periodsPerYear)
	if err != nil {
		return money.Precise{}, err
	}
	n, err := integer(periodsPerYear)
	if err != nil {
		return money.Precise{}, err
	}
	return rate.Mul(n)
}

// rateForFraction is the rate equivalent to ea over fraction f of a year: (1 + ea)^f − 1, compounded and
// never f × ea. With f the YearFraction of a day it is the rate of that day under any convention.
func rateForFraction(ea, f money.Precise) (money.Precise, error) {
	one, err := integer(1)
	if err != nil {
		return money.Precise{}, err
	}
	base, err := ea.Add(one)
	if err != nil {
		return money.Precise{}, err
	}
	if base.Cmp(money.Precise{}) <= 0 {
		return money.Precise{}, fmt.Errorf("%w: %s", ErrInvalidRate, ea)
	}
	factor, err := base.Pow(f)
	if err != nil {
		return money.Precise{}, err
	}
	minusOne, err := integer(-1)
	if err != nil {
		return money.Precise{}, err
	}
	return factor.Add(minusOne)
}
