package domain

import (
	"cmp"
	"errors"
	"fmt"
	"strconv"

	"github.com/DrewGGM/aluna/internal/money"
)

var (
	// ErrLimitExceeded means a movement that would exceed a limit of the product (FR-24): it is rejected whole.
	// The error is a *LimitError, which names the limit.
	ErrLimitExceeded = errors.New("limit exceeded")
	// ErrSMMLVOutsideCOP means a limit in SMMLV on an account that is not in COP: the minimum wage is in pesos.
	ErrSMMLVOutsideCOP = errors.New("a limit in SMMLV applies only to an account in COP")
	// ErrMinimumWageTooLow means a minimum wage so low that a limit in SMMLV is less than one minor unit: the
	// market parameter is wrong, not the movement.
	ErrMinimumWageTooLow = errors.New("the minimum wage makes a limit in SMMLV less than one minor unit")
)

// Names of the limits, as the product definition calls them.
const (
	limitPerOperation  = "per_operation"
	limitDailyDebits   = "daily_debits"
	limitMonthlyDebits = "monthly_debits"
	limitMaxBalance    = "max_balance"
)

// Limit is a limit of the product: an amount in minor units or a number of SMMLV, only one of the two; the
// zero Limit does not apply.
type Limit struct{ Units, SMMLV int64 }

// Limits are the product's limits: per operation (a debit or a credit), of the debits of the day and of the
// month, and of the balance.
type Limits struct{ PerOperation, DailyDebits, MonthlyDebits, MaxBalance Limit }

// Usage is what an account used of its limits before a movement, in minor units: its debits of the day and of
// the month and its balance.
type Usage struct{ DailyDebits, MonthlyDebits, Balance int64 }

// Caps are an account's limits in minor units of its currency on a day; 0 does not apply (a limit is at least
// one unit).
type Caps struct{ PerOperation, DailyDebits, MonthlyDebits, MaxBalance int64 }

// LimitError is a movement over the limit Name, of Units minor units; it is ErrLimitExceeded.
type LimitError struct {
	Name  string
	Units int64
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("%v: %s of %d minor units", ErrLimitExceeded, e.Name, e.Units)
}

// Is makes a LimitError ErrLimitExceeded.
func (e *LimitError) Is(target error) bool { return target == ErrLimitExceeded }

// UsesSMMLV reports whether a limit is in SMMLV: only then is the minimum wage needed.
func (l Limits) UsesSMMLV() bool {
	return l.PerOperation.SMMLV != 0 || l.DailyDebits.SMMLV != 0 || l.MonthlyDebits.SMMLV != 0 || l.MaxBalance.SMMLV != 0
}

// InUnits converts l to minor units of c; a limit in SMMLV is that many times smmlv, the minimum wage in
// pesos of the day, rounded down: only in COP (ErrSMMLVOutsideCOP otherwise) and at least one unit
// (ErrMinimumWageTooLow otherwise).
func (l Limits) InUnits(c money.Currency, smmlv money.Precise) (Caps, error) {
	var caps Caps
	var errs [4]error
	caps.PerOperation, errs[0] = l.PerOperation.inUnits(c, smmlv)
	caps.DailyDebits, errs[1] = l.DailyDebits.inUnits(c, smmlv)
	caps.MonthlyDebits, errs[2] = l.MonthlyDebits.inUnits(c, smmlv)
	caps.MaxBalance, errs[3] = l.MaxBalance.inUnits(c, smmlv)
	if err := errors.Join(errs[:]...); err != nil {
		return Caps{}, err
	}
	return caps, nil
}

func (l Limit) inUnits(c money.Currency, smmlv money.Precise) (int64, error) {
	switch {
	case l.SMMLV == 0:
		return l.Units, nil
	case c.Code() != "COP":
		return 0, fmt.Errorf("%w: %d SMMLV in %s", ErrSMMLVOutsideCOP, l.SMMLV, c.Code())
	}
	times, err := money.ParsePrecise(strconv.FormatInt(l.SMMLV, 10))
	if err != nil {
		return 0, err
	}
	pesos, err := smmlv.Mul(times)
	if err != nil {
		return 0, err
	}
	amount, _, err := pesos.ToAmount(c, money.Down)
	if err != nil {
		return 0, err
	}
	if amount.Units() < 1 {
		return 0, fmt.Errorf("%w: %d SMMLV of %s pesos", ErrMinimumWageTooLow, l.SMMLV, smmlv)
	}
	return amount.Units(), nil
}

// Unlimited reports whether no limit applies: then nothing used needs to be read.
func (c Caps) Unlimited() bool { return c == Caps{} }

// CheckDebit is ErrLimitExceeded if a debit of units, after u, exceeds the limit per operation or that of the
// debits of the day or of the month.
func (c Caps) CheckDebit(u Usage, units int64) error {
	return cmp.Or(
		check(limitPerOperation, c.PerOperation, 0, units),
		check(limitDailyDebits, c.DailyDebits, u.DailyDebits, units),
		check(limitMonthlyDebits, c.MonthlyDebits, u.MonthlyDebits, units),
	)
}

// CheckCredit is ErrLimitExceeded if a credit of units, after u, exceeds the limit per operation or the
// maximum balance.
func (c Caps) CheckCredit(u Usage, units int64) error {
	return cmp.Or(
		check(limitPerOperation, c.PerOperation, 0, units),
		check(limitMaxBalance, c.MaxBalance, u.Balance, units),
	)
}

// check is a LimitError if used + units exceeds a cap that applies; used, units and the cap are not
// negative, so the difference does not overflow.
func check(name string, limit, used, units int64) error {
	if limit > 0 && units > limit-used {
		return &LimitError{Name: name, Units: limit}
	}
	return nil
}
