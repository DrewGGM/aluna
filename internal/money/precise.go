package money

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/cockroachdb/apd/v3"
)

var (
	// ErrInvalidDecimal indica un texto que no es un decimal finito, un argumento fuera de rango (una
	// escala negativa) o una operación sin resultado real (la raíz de un negativo).
	ErrInvalidDecimal = errors.New("decimal inválido")
	// ErrDivisionByZero indica una división entre cero, también la implícita de 0 elevado a un negativo.
	ErrDivisionByZero = errors.New("división entre cero")
	// ErrInvalidRounding indica un RoundingMode que no existe.
	ErrInvalidRounding = errors.New("modo de redondeo inválido")
)

// RoundingMode dice cómo se redondea al pasar de un decimal a menos cifras. El valor cero es HalfEven,
// el modo por defecto (ADR-0005).
type RoundingMode int

const (
	// HalfEven va al más cercano y, en empate, al par: 0,5 → 0 y 1,5 → 2 (redondeo bancario).
	HalfEven RoundingMode = iota
	// HalfUp va al más cercano y, en empate, lejos de cero: 2,5 → 3 y -2,5 → -3.
	HalfUp
	// Down trunca hacia cero: 2,9 → 2 y -2,9 → -2.
	Down
	// Up se aleja de cero: 2,1 → 3 y -2,1 → -3.
	Up
)

// maxExponent acota un Precise a ±10^100 y a 100 decimales: ningún monto ni tasa real se acerca, y el
// límite impide que un "1e999999999" convierta una división en una reserva de memoria enorme.
const maxExponent = 100

// powDigits es la precisión de Pow, la única operación que redondea a una precisión que no elige
// quien llama: 34 dígitos significativos, como decimal128.
const powDigits = 34

// exact suma y multiplica sin redondear (precisión 0 en apd es ilimitada); approx calcula Pow.
var (
	exact  = apd.Context{MaxExponent: maxExponent, MinExponent: -maxExponent, Traps: apd.DefaultTraps}
	approx = apd.Context{Precision: powDigits, MaxExponent: maxExponent, MinExponent: -maxExponent, Traps: apd.DefaultTraps}
)

// Precise es un decimal de precisión arbitraria para el cálculo intermedio (intereses, tasas,
// prorrateos), en unidades mayores de la moneda: 100.000 COP es 100000. Es inmutable, su valor cero es
// 0 y no expone el decimal que lo respalda. Se compara con Cmp, no con ==. Se vuelve dinero solo con
// ToAmount, que redondea con un modo explícito.
type Precise struct {
	d *apd.Decimal
	_ [0]func() // no comparable: == compararía punteros y diría que 0 ≠ 0
}

// zeroDecimal es el valor de un Precise cero. Es de solo lectura: nunca es destino de una operación.
var zeroDecimal apd.Decimal

func (p Precise) dec() *apd.Decimal {
	if p.d == nil {
		return &zeroDecimal
	}
	return p.d
}

// ParsePrecise lee un decimal como "0.004" o "-12.5e-3". Rechaza NaN, infinitos, la coma decimal,
// valores fuera de ±10^100 y más de 100 decimales.
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

// Precise es el monto en unidades mayores de su moneda, sin pérdida.
func (a Amount) Precise() Precise {
	return Precise{d: apd.New(a.units, -int32(a.cur.exponent))} //nolint:gosec // los exponentes ISO 4217 son de un dígito
}

// String es el decimal sin exponente, para logs y mensajes de error.
func (p Precise) String() string { return p.dec().Text('f') }

// Cmp devuelve -1, 0 o 1 según p sea menor, igual o mayor que q; 1,50 y 1,5 son iguales.
func (p Precise) Cmp(q Precise) int { return p.dec().Cmp(q.dec()) }

// Add es p + q, exacto. Falla con ErrOverflow si el resultado sale de ±10^100.
func (p Precise) Add(q Precise) (Precise, error) {
	d := new(apd.Decimal)
	cond, err := exact.Add(d, p.dec(), q.dec())
	return result(d, "+", cond, err)
}

// Mul es p × q, exacto. Falla con ErrOverflow si el resultado sale de ±10^100 o necesita más de 100
// decimales.
func (p Precise) Mul(q Precise) (Precise, error) {
	d := new(apd.Decimal)
	cond, err := exact.Mul(d, p.dec(), q.dec())
	return result(d, "×", cond, err)
}

// Pow es p elevado a q con 34 dígitos significativos: (1 + tasa)^(días/365). Un exponente no entero
// exige p > 0, y 0 elevado a un negativo es ErrDivisionByZero.
func (p Precise) Pow(q Precise) (Precise, error) {
	d := new(apd.Decimal)
	cond, err := approx.Pow(d, p.dec(), q.dec())
	return result(d, "^", cond, err)
}

// Quo es p / q redondeado a scale decimales con m, en un solo redondeo del cociente exacto (dividir
// con precisión fija y luego redondear redondea dos veces y puede errar en el último dígito).
func (p Precise) Quo(q Precise, scale int, m RoundingMode) (Precise, error) {
	quotient, err := p.scaledQuo(q, scale, m)
	if err != nil {
		return Precise{}, err
	}
	d := apd.NewWithBigInt(new(apd.BigInt).SetMathBigInt(quotient), -int32(scale)) //nolint:gosec // scaledQuo acota scale a maxExponent
	return result(d, "/", 0, nil)
}

// scaledQuo es p/q·10^scale redondeado a entero con m.
func (p Precise) scaledQuo(q Precise, scale int, m RoundingMode) (*big.Int, error) {
	if scale < 0 || scale > maxExponent {
		return nil, fmt.Errorf("%w: escala %d fuera de 0…%d", ErrInvalidDecimal, scale, maxExponent)
	}
	num, den := coefficient(p.dec()), coefficient(q.dec())
	if den.Sign() == 0 {
		return nil, ErrDivisionByZero
	}
	// p/q·10^scale = (num·10^pe) / (den·10^qe) · 10^scale: el factor 10^shift pasa a quien corresponda.
	shift := int(p.dec().Exponent) - int(q.dec().Exponent) + scale
	if shift >= 0 {
		num.Mul(num, pow10(shift))
	} else {
		den.Mul(den, pow10(-shift))
	}
	return divRound(num, den, m)
}

// ToAmount redondea p a unidades menores de c con m y devuelve también el residuo exacto
// (p − monto), para registrarlo o acumularlo: monto + residuo == p siempre. Falla con ErrOverflow si
// el monto no cabe en int64 y con ErrUnknownCurrency si c es el valor cero.
func (p Precise) ToAmount(c Currency, m RoundingMode) (Amount, Precise, error) {
	if c.code == "" {
		return Amount{}, Precise{}, fmt.Errorf("%w: sin moneda", ErrUnknownCurrency)
	}
	// Dividir entre 1 a la escala de la moneda es redondear a sus unidades menores.
	units, err := p.scaledQuo(Precise{d: apd.New(1, 0)}, c.exponent, m)
	if err != nil {
		return Amount{}, Precise{}, err
	}
	if !units.IsInt64() {
		return Amount{}, Precise{}, fmt.Errorf("%w: el monto no cabe en int64 unidades de %s", ErrOverflow, c.code)
	}
	return New(units.Int64(), c), residue(p.dec(), units, -c.exponent), nil
}

// residue es d − units·10^exponent, calculado con enteros en el menor de los dos exponentes: es exacto
// y no puede fallar, y tiene a lo más los decimales de d.
func residue(d *apd.Decimal, units *big.Int, exponent int) Precise {
	low := min(int(d.Exponent), exponent)
	r := coefficient(d)
	r.Mul(r, pow10(int(d.Exponent)-low))
	r.Sub(r, new(big.Int).Mul(units, pow10(exponent-low)))
	return Precise{d: apd.NewWithBigInt(new(apd.BigInt).SetMathBigInt(r), int32(low))} //nolint:gosec // low ≥ -maxExponent
}

// divRound divide num entre den y redondea el cociente a entero con m.
func divRound(num, den *big.Int, m RoundingMode) (*big.Int, error) {
	if den.Sign() < 0 {
		num.Neg(num)
		den.Neg(den)
	}
	q, r := new(big.Int).QuoRem(num, den, new(big.Int)) // trunca hacia cero; r lleva el signo de num
	var away bool
	switch m {
	case Down: // QuoRem ya truncó hacia cero
	case Up:
		away = r.Sign() != 0
	case HalfEven, HalfUp:
		half := new(big.Int).Lsh(new(big.Int).Abs(r), 1).Cmp(den) // 2|r| contra den
		away = half > 0 || (half == 0 && (m == HalfUp || q.Bit(0) == 1))
	default:
		return nil, fmt.Errorf("%w: %d", ErrInvalidRounding, m)
	}
	if away {
		q.Add(q, big.NewInt(int64(num.Sign())))
	}
	return q, nil
}

// coefficient es el coeficiente con signo de d, en un big.Int nuevo.
func coefficient(d *apd.Decimal) *big.Int {
	c := d.Coeff.MathBigInt()
	if d.Negative {
		c.Neg(c)
	}
	return c
}

func pow10(n int) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil) }

// result traduce el resultado de apd al de money. apd no levanta condición en 0^-n (da infinito, por
// la especificación GDA), así que un resultado no finito se rechaza aparte: si no, ToAmount lo leería
// como 0. El cero negativo se normaliza para que String no muestre "-0".
func result(d *apd.Decimal, op string, cond apd.Condition, err error) (Precise, error) {
	switch {
	case cond.Overflow() || cond.Underflow() || cond.Subnormal() || (err == nil && !inRange(d)):
		return Precise{}, fmt.Errorf("%w: resultado de %s fuera de ±10^%d o con más de %d decimales", ErrOverflow, op, maxExponent, maxExponent)
	case err != nil:
		return Precise{}, fmt.Errorf("%w: %s: %v", ErrInvalidDecimal, op, err) //nolint:errorlint // el error de apd es texto, no un sentinel
	case d.Form != apd.Finite:
		return Precise{}, fmt.Errorf("%w: resultado infinito en %s", ErrDivisionByZero, op)
	}
	if d.IsZero() {
		d.Negative = false
	}
	return Precise{d: d}, nil
}

// inRange dice si un decimal finito cabe en ±10^100 con a lo más 100 decimales. Un producto de dos
// valores con 60 decimales tiene 120 aunque su magnitud esté en rango, por eso no basta el límite de
// exponente de apd, que mira solo la magnitud.
func inRange(d *apd.Decimal) bool {
	if d.Form != apd.Finite || d.IsZero() {
		return true
	}
	return d.Exponent >= -maxExponent && d.NumDigits()+int64(d.Exponent)-1 <= maxExponent
}
