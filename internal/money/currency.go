package money

import (
	"errors"
	"fmt"
)

// ErrUnknownCurrency means a code that is not in the table of supported currencies.
var ErrUnknownCurrency = errors.New("unknown currency")

// Currency is an ISO 4217 currency with its exponent: how many digits the minor unit has (COP = 2: one
// peso is 100 units). The zero value is not a currency: Add and Sub reject it.
type Currency struct {
	code     string
	exponent int
}

// exponents are the supported currencies. A new currency is added here with its ISO 4217 exponent.
var exponents = map[string]int{
	"COP": 2,
	"USD": 2,
	"EUR": 2,
	"JPY": 0,
	"CLF": 4, // Chilean Unidad de Fomento
}

// CurrencyOf returns the currency with ISO 4217 code code. It does not normalize: the code arrives
// already validated from the edge (API, files), so "cop" is an error and not a COP.
func CurrencyOf(code string) (Currency, error) {
	exponent, ok := exponents[code]
	if !ok {
		return Currency{}, fmt.Errorf("%w: %q", ErrUnknownCurrency, code)
	}
	return Currency{code: code, exponent: exponent}, nil
}

// Code is the currency's ISO 4217 code.
func (c Currency) Code() string { return c.code }
