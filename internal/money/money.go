// Package money es el kernel de dinero (ADR-0005): montos enteros en unidades menores de una moneda,
// con aritmética que falla ante overflow o mezcla de monedas en vez de dar un resultado equivocado.
package money

import (
	"cmp"
	"errors"
	"fmt"
	"math"
)

var (
	// ErrCurrencyMismatch indica una operación entre montos de monedas distintas.
	ErrCurrencyMismatch = errors.New("monedas distintas")
	// ErrOverflow indica que el resultado no cabe en int64.
	ErrOverflow = errors.New("overflow de monto")
)

// Amount es un monto en unidades menores de su moneda: 100.000 COP es New(10_000_000, cop).
type Amount struct {
	units int64
	cur   Currency
}

// New crea un monto de units unidades menores de c.
func New(units int64, c Currency) Amount { return Amount{units: units, cur: c} }

// Units es el monto en unidades menores.
func (a Amount) Units() int64 { return a.units }

// Currency es la moneda del monto.
func (a Amount) Currency() Currency { return a.cur }

// IsZero dice si el monto es cero.
func (a Amount) IsZero() bool { return a.units == 0 }

// Sign devuelve -1, 0 o 1 según el signo del monto.
func (a Amount) Sign() int { return cmp.Compare(a.units, 0) }

// Add suma b a a. Falla con ErrCurrencyMismatch si las monedas difieren o falta la moneda, y con
// ErrOverflow si el resultado no cabe en int64; nunca da la vuelta.
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

// Sub resta b de a, con los mismos errores que Add. No se calcula como a + (-b): -MinInt64 no existe,
// pero -1 - MinInt64 = MaxInt64 sí es un resultado válido.
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

// Neg cambia el signo del monto. MinInt64 no tiene opuesto en int64.
func (a Amount) Neg() (Amount, error) {
	if a.units == math.MinInt64 {
		return Amount{}, fmt.Errorf("%w: -(%d)", ErrOverflow, a.units)
	}
	return New(-a.units, a.cur), nil
}

// sameCurrency también rechaza el monto sin moneda: Amount{} es lo que devuelve un error descartado y
// no debe poder acumularse en silencio.
func (a Amount) sameCurrency(b Amount, op string) error {
	if a.cur != b.cur || a.cur.code == "" {
		return fmt.Errorf("%w: %q %s %q", ErrCurrencyMismatch, a.cur.code, op, b.cur.code)
	}
	return nil
}
