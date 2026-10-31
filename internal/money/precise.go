package money

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/cockroachdb/apd/v3"
)

var (
	// ErrInvalidDecimal means text that is not a finite decimal, an out-of-range argument (a negative
	// scale) or an operation with no real result (the root of a negative number).
	ErrInvalidDecimal = errors.New("invalid decimal")
	// ErrDivisionByZero means a division by zero, including the implicit one of 0 raised to a negative.
	ErrDivisionByZero = errors.New("division by zero")
	// ErrInvalidRounding means a RoundingMode that does not exist.
	ErrInvalidRounding = errors.New("invalid rounding mode")
)

// RoundingMode says how to round when going from a decimal to fewer digits. The zero value is HalfEven,
// the default mode (ADR-0005).
type RoundingMode int

const (
	// HalfEven goes to the nearest and, on a tie, to the even one: 0.5 → 0 and 1.5 → 2 (banker's rounding).
	HalfEven RoundingMode = iota
	// HalfUp goes to the nearest and, on a tie, away from zero: 2.5 → 3 and -2.5 → -3.
	HalfUp
	// Down truncates toward zero: 2.9 → 2 and -2.9 → -2.
	Down
	// Up moves away from zero: 2.1 → 3 and -2.1 → -3.
	Up
)

// maxExponent bounds a Precise to ±10^100 and to 100 decimal places: no real amount or rate comes close,
// and the limit keeps a "1e999999999" from turning a division into a huge memory allocation.
const maxExponent = 100

// powDigits is the precision of Pow, the only operation that rounds to a precision the caller does not
// choose: 34 significant digits, like decimal128.
const powDigits = 34

// exact adds and multiplies without rounding (precision 0 in apd is unlimited); approx computes Pow.
var (
	exact  = apd.Context{MaxExponent: maxExponent, MinExponent: -maxExponent, Traps: apd.DefaultTraps}
	approx = apd.Context{Precision: powDigits, MaxExponent: maxExponent, MinExponent: -maxExponent, Traps: apd.DefaultTraps}
)

// Precise is an arbitrary-precision decimal for intermediate calculation (interest, rates, prorations),
// in the currency's major units: 100,000 COP is 100000. It is immutable, its zero value is 0 and it does
// not expose its backing decimal. It is compared with Cmp, not with ==. It becomes money only through
// ToAmount, which rounds with an explicit mode.
type Precise struct {
	d *apd.Decimal
	_ [0]func() // not comparable: == would compare pointers and say that 0 ≠ 0
}

// zeroDecimal is the value of a zero Precise. It is read-only: it is never the target of an operation.
var zeroDecimal apd.Decimal

func (p Precise) dec() *apd.Decimal {
	if p.d == nil {
		return &zeroDecimal
	}
	return p.d
}

// ParsePrecise reads a decimal such as "0.004" or "-12.5e-3". It rejects NaN, infinities, the decimal
// comma, values outside ±10^100 and more than 100 decimal places.
func ParsePrecise(s string) (Precise, error) {
	d, _, err := exact.NewFromString(s)
	if err != nil || d.Form != apd.Finite || d.Exponent < -maxExponent {
		return Precise{}, fmt.Errorf("%w: %q", ErrInvalidDecimal, s)
	}
	if d.IsZero() {
		d.Negative = false
	}
	return Precise{d: d}, nil
}

// Precise is the amount in its currency's major units, without loss.
func (a Amount) Precise() Precise {
	return Precise{d: apd.New(a.units, -int32(a.cur.exponent))} //nolint:gosec // ISO 4217 exponents are one digit
}

// String is the decimal without exponent, for logs and error messages.
func (p Precise) String() string { return p.dec().Text('f') }

// Cmp returns -1, 0 or 1 as p is less than, equal to or greater than q; 1.50 and 1.5 are equal.
func (p Precise) Cmp(q Precise) int { return p.dec().Cmp(q.dec()) }

// Add is p + q, exact. It fails with ErrOverflow if the result falls outside ±10^100.
func (p Precise) Add(q Precise) (Precise, error) {
	d := new(apd.Decimal)
	cond, err := exact.Add(d, p.dec(), q.dec())
	return result(d, "+", cond, err)
}

// Mul is p × q, exact. It fails with ErrOverflow if the result falls outside ±10^100 or needs more than
// 100 decimal places.
func (p Precise) Mul(q Precise) (Precise, error) {
	d := new(apd.Decimal)
	cond, err := exact.Mul(d, p.dec(), q.dec())
	return result(d, "×", cond, err)
}

// Pow is p raised to q with 34 significant digits: (1 + rate)^(days/365). A non-integer exponent
// requires p > 0, and 0 raised to a negative is ErrDivisionByZero.
func (p Precise) Pow(q Precise) (Precise, error) {
	d := new(apd.Decimal)
	cond, err := approx.Pow(d, p.dec(), q.dec())
	return result(d, "^", cond, err)
}

// Quo is p / q rounded to scale decimal places with m, in a single rounding of the exact quotient
// (dividing with fixed precision and then rounding rounds twice and can be off in the last digit).
func (p Precise) Quo(q Precise, scale int, m RoundingMode) (Precise, error) {
	quotient, err := p.scaledQuo(q, scale, m)
	if err != nil {
		return Precise{}, err
	}
	d := apd.NewWithBigInt(new(apd.BigInt).SetMathBigInt(quotient), -int32(scale)) //nolint:gosec // scaledQuo bounds scale to maxExponent
	return result(d, "/", 0, nil)
}

// scaledQuo is p/q·10^scale rounded to an integer with m.
func (p Precise) scaledQuo(q Precise, scale int, m RoundingMode) (*big.Int, error) {
	if scale < 0 || scale > maxExponent {
		return nil, fmt.Errorf("%w: scale %d outside 0…%d", ErrInvalidDecimal, scale, maxExponent)
	}
	num, den := coefficient(p.dec()), coefficient(q.dec())
	if den.Sign() == 0 {
		return nil, ErrDivisionByZero
	}
	// p/q·10^scale = (num·10^pe) / (den·10^qe) · 10^scale: the 10^shift factor goes to whichever side
	// needs it.
	shift := int(p.dec().Exponent) - int(q.dec().Exponent) + scale
	if shift >= 0 {
		num.Mul(num, pow10(shift))
	} else {
		den.Mul(den, pow10(-shift))
	}
	return divRound(num, den, m)
}

// ToAmount rounds p to minor units of c with m and also returns the exact residue (p − amount), to
// record or accumulate it: amount + residue == p always. It fails with ErrOverflow if the amount does
// not fit in int64 and with ErrUnknownCurrency if c is the zero value.
func (p Precise) ToAmount(c Currency, m RoundingMode) (Amount, Precise, error) {
	if c.code == "" {
		return Amount{}, Precise{}, fmt.Errorf("%w: no currency", ErrUnknownCurrency)
	}
	// Dividing by 1 at the currency's scale is rounding to its minor units.
	units, err := p.scaledQuo(Precise{d: apd.New(1, 0)}, c.exponent, m)
	if err != nil {
		return Amount{}, Precise{}, err
	}
	if !units.IsInt64() {
		return Amount{}, Precise{}, fmt.Errorf("%w: amount does not fit in int64 units of %s", ErrOverflow, c.code)
	}
	return New(units.Int64(), c), residue(p.dec(), units, -c.exponent), nil
}

// residue is d − units·10^exponent, computed with integers at the smaller of the two exponents: it is
// exact and cannot fail, and has at most d's decimal places.
func residue(d *apd.Decimal, units *big.Int, exponent int) Precise {
	low := min(int(d.Exponent), exponent)
	r := coefficient(d)
	r.Mul(r, pow10(int(d.Exponent)-low))
	r.Sub(r, new(big.Int).Mul(units, pow10(exponent-low)))
	return Precise{d: apd.NewWithBigInt(new(apd.BigInt).SetMathBigInt(r), int32(low))} //nolint:gosec // low ≥ -maxExponent
}

// divRound divides num by den and rounds the quotient to an integer with m.
func divRound(num, den *big.Int, m RoundingMode) (*big.Int, error) {
	if den.Sign() < 0 {
		num.Neg(num)
		den.Neg(den)
	}
	q, r := new(big.Int).QuoRem(num, den, new(big.Int)) // truncates toward zero; r carries num's sign
	var away bool
	switch m {
	case Down: // QuoRem already truncated toward zero
	case Up:
		away = r.Sign() != 0
	case HalfEven, HalfUp:
		half := new(big.Int).Lsh(new(big.Int).Abs(r), 1).Cmp(den) // 2|r| against den
		away = half > 0 || (half == 0 && (m == HalfUp || q.Bit(0) == 1))
	default:
		return nil, fmt.Errorf("%w: %d", ErrInvalidRounding, m)
	}
	if away {
		q.Add(q, big.NewInt(int64(num.Sign())))
	}
	return q, nil
}

// coefficient is d's signed coefficient, in a new big.Int.
func coefficient(d *apd.Decimal) *big.Int {
	c := d.Coeff.MathBigInt()
	if d.Negative {
		c.Neg(c)
	}
	return c
}

func pow10(n int) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil) }

// result translates apd's result into money's. apd raises no condition on 0^-n (it gives infinity, per
// the GDA specification), so a non-finite result is rejected separately: otherwise, ToAmount would read
// it as 0. Negative zero is normalized so that String does not show "-0".
func result(d *apd.Decimal, op string, cond apd.Condition, err error) (Precise, error) {
	switch {
	case cond.Overflow() || cond.Underflow() || cond.Subnormal() || (err == nil && !inRange(d)):
		return Precise{}, fmt.Errorf("%w: result of %s outside ±10^%d or with more than %d decimal places", ErrOverflow, op, maxExponent, maxExponent)
	case err != nil:
		return Precise{}, fmt.Errorf("%w: %s: %v", ErrInvalidDecimal, op, err) //nolint:errorlint // apd's error is text, not a sentinel
	case d.Form != apd.Finite:
		return Precise{}, fmt.Errorf("%w: infinite result in %s", ErrDivisionByZero, op)
	}
	if d.IsZero() {
		d.Negative = false
	}
	return Precise{d: d}, nil
}

// inRange reports whether a finite decimal fits in ±10^100 with at most 100 decimal places. A product
// of two values with 60 decimal places has 120 even if its magnitude is in range, which is why apd's
// exponent limit, which only looks at magnitude, is not enough.
func inRange(d *apd.Decimal) bool {
	if d.Form != apd.Finite || d.IsZero() {
		return true
	}
	return d.Exponent >= -maxExponent && d.NumDigits()+int64(d.Exponent)-1 <= maxExponent
}
