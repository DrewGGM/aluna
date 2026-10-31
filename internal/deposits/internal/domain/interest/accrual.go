package interest

import (
	"slices"
	"time"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/products"
)

// accrualScale is the number of decimals of a minor unit each day's interest keeps: it bounds the digits a
// year of sums grows to (exact products of Pow results would not fit, Q-44) and is far below a minor unit.
const accrualScale = 20

// AccrualState is what an account accrued and posted since it was opened, both in minor units of its
// currency, like its balance and the tiers (not in major units, as a Precise usually is): the accrual keeps
// every decimal, the posting whole units. Their difference is the interest accrued and not posted yet,
// residue included.
type AccrualState struct {
	AccruedPrecise money.Precise
	PostedUnits    int64
}

// AccrueDay accrues the interest of day and returns the new state and the day's interest, in minor units.
// The base is the balance plus the interest accrued and not posted yet, which earns interest with it: the
// rate of the day is the EA compounded over the day's year fraction under dc, so a year of accrual on a
// constant balance is the EA. Each tier's rate applies to the part of the base in its slab, from its start
// (excluded) to the next one's; a base of zero or less earns nothing.
//
// The days are accrued in order (each one compounds on the previous ones), and balanceUnits is the balance
// that already includes every posting state counts: a capitalization posted after the balance was read
// would be left out of it and earn nothing that day.
func AccrueDay(state AccrualState, balanceUnits int64, tiers []products.RateTier, dc products.DayCount, day calendar.Date) (AccrualState, money.Precise, error) {
	f, err := YearFraction(dc, day, nextDay(day))
	if err != nil {
		return AccrualState{}, money.Precise{}, err
	}
	balance, err := precise64(balanceUnits)
	if err != nil {
		return AccrualState{}, money.Precise{}, err
	}
	posted, err := precise64(state.PostedUnits)
	if err != nil {
		return AccrualState{}, money.Precise{}, err
	}
	withAccrued, err := balance.Add(state.AccruedPrecise)
	if err != nil {
		return AccrualState{}, money.Precise{}, err
	}
	base, err := minus(withAccrued, posted)
	if err != nil {
		return AccrualState{}, money.Precise{}, err
	}
	interest, err := slabInterest(base, tiers, f)
	if err != nil {
		return AccrualState{}, money.Precise{}, err
	}
	if state.AccruedPrecise, err = state.AccruedPrecise.Add(interest); err != nil {
		return AccrualState{}, money.Precise{}, err
	}
	return state, interest, nil
}

// slabInterest adds, for each tier from the highest down, the part of base in its slab times the tier's
// rate for fraction f: each slab ends where the one above it starts.
func slabInterest(base money.Precise, tiers []products.RateTier, f money.Precise) (money.Precise, error) {
	var interest money.Precise
	top := base
	for _, t := range slices.Backward(tiers) {
		from, err := precise64(t.FromUnits)
		if err != nil {
			return money.Precise{}, err
		}
		if top.Cmp(from) <= 0 {
			continue
		}
		part, err := slabPart(top, from, t.RateEA, f)
		if err != nil {
			return money.Precise{}, err
		}
		if interest, err = interest.Add(part); err != nil {
			return money.Precise{}, err
		}
		top = from
	}
	return interest, nil
}

// slabPart is (top − from) × ((1 + ea)^f − 1), rounded to accrualScale.
func slabPart(top, from, ea, f money.Precise) (money.Precise, error) {
	portion, err := minus(top, from)
	if err != nil {
		return money.Precise{}, err
	}
	rate, err := rateForFraction(ea, f)
	if err != nil {
		return money.Precise{}, err
	}
	part, err := portion.Mul(rate)
	if err != nil {
		return money.Precise{}, err
	}
	one, err := precise64(1)
	if err != nil {
		return money.Precise{}, err
	}
	return part.Quo(one, accrualScale, money.HalfEven)
}

// DueForPosting rounds the accrual to minor units of cur with mode and returns what is due to post (that
// rounded accrual less what was posted) and the state once posted. The residue, below one minor unit, stays
// in the accrual for the next posting; posting again without new interest is 0. The new state is committed
// with the ledger posting, in the same transaction, and mode is the same for the account's whole life
// (changing it could make the rounded accrual go back below what was posted).
func DueForPosting(state AccrualState, cur money.Currency, mode money.RoundingMode) (int64, AccrualState, error) {
	// one minor unit in major units: it scales the accrual by 10^−exponent before ToAmount rounds it
	major, err := state.AccruedPrecise.Mul(money.New(1, cur).Precise())
	if err != nil {
		return 0, AccrualState{}, err
	}
	total, _, err := major.ToAmount(cur, mode)
	if err != nil {
		return 0, AccrualState{}, err
	}
	due := total.Units() - state.PostedUnits
	state.PostedUnits = total.Units()
	return due, state, nil
}

func minus(a, b money.Precise) (money.Precise, error) {
	minusOne, err := precise64(-1)
	if err != nil {
		return money.Precise{}, err
	}
	negative, err := b.Mul(minusOne)
	if err != nil {
		return money.Precise{}, err
	}
	return a.Add(negative)
}

func nextDay(d calendar.Date) calendar.Date {
	t := time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
	return calendar.Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}
}
