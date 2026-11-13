package interest

import (
	"errors"
	"fmt"

	"github.com/DrewGGM/aluna/internal/money"
)

// ErrInvalidRate means an EA of -100 % or less, which has no equivalent rate.
var ErrInvalidRate = errors.New("EA is -100 % or less")

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
