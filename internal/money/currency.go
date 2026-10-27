package money

import (
	"errors"
	"fmt"
)

// ErrUnknownCurrency indica un código que no está en la tabla de monedas soportadas.
var ErrUnknownCurrency = errors.New("moneda desconocida")

// Currency es una moneda ISO 4217 con su exponente: cuántos dígitos tiene la unidad menor (COP = 2:
// un peso son 100 unidades). El valor cero no es una moneda: Add y Sub lo rechazan.
type Currency struct {
	code     string
	exponent int
}

// exponents son las monedas soportadas. Una moneda nueva se agrega aquí con su exponente ISO 4217.
var exponents = map[string]int{
	"COP": 2,
	"USD": 2,
	"EUR": 2,
	"JPY": 0,
	"CLF": 4, // Unidad de Fomento chilena
}

// CurrencyOf devuelve la moneda con código ISO 4217 code. No normaliza: el código llega ya validado
// desde el borde (API, archivos), así que "cop" es un error y no un COP.
func CurrencyOf(code string) (Currency, error) {
	exponent, ok := exponents[code]
	if !ok {
		return Currency{}, fmt.Errorf("%w: %q", ErrUnknownCurrency, code)
	}
	return Currency{code: code, exponent: exponent}, nil
}

// Code es el código ISO 4217 de la moneda.
func (c Currency) Code() string { return c.code }
