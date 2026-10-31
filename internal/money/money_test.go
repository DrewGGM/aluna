package money

import (
	"errors"
	"math"
	"testing"
)

func mustCurrency(t *testing.T, code string) Currency {
	t.Helper()
	c, err := CurrencyOf(code)
	if err != nil {
		t.Fatalf("CurrencyOf(%q): %v", code, err)
	}
	return c
}

func TestCurrencyOf(t *testing.T) {
	exponents := map[string]int{"COP": 2, "USD": 2, "EUR": 2, "JPY": 0, "CLF": 4}
	for code, want := range exponents {
		c := mustCurrency(t, code)
		if c.Code() != code || c.exponent != want {
			t.Errorf("CurrencyOf(%q) = %s/%d, want %s/%d", code, c.Code(), c.exponent, code, want)
		}
	}
}

func TestCurrencyOfUnknown(t *testing.T) {
	for _, code := range []string{"", "XXX", "cop", "COPX"} {
		if _, err := CurrencyOf(code); !errors.Is(err, ErrUnknownCurrency) {
			t.Errorf("CurrencyOf(%q) error = %v, want ErrUnknownCurrency", code, err)
		}
	}
}

func TestAccessors(t *testing.T) {
	cop := mustCurrency(t, "COP")

	a := New(10_000_000, cop) // 100,000 pesos

	if a.Units() != 10_000_000 || a.Currency() != cop {
		t.Fatalf("New = %d %s", a.Units(), a.Currency().Code())
	}
}

func TestAdd(t *testing.T) {
	cop := mustCurrency(t, "COP")
	cases := map[string]struct {
		a, b    int64
		want    int64
		wantErr error
	}{
		"positive":            {150, 250, 400, nil},
		"with negative":       {150, -250, -100, nil},
		"MaxInt64 + 1":        {math.MaxInt64, 1, 0, ErrOverflow},
		"MinInt64 + -1":       {math.MinInt64, -1, 0, ErrOverflow},
		"MaxInt64 + MinInt64": {math.MaxInt64, math.MinInt64, -1, nil},
		"zero":                {0, 0, 0, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := New(tc.a, cop).Add(New(tc.b, cop))

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if err == nil && got != New(tc.want, cop) {
				t.Fatalf("Add = %d %s, want %d COP", got.Units(), got.Currency().Code(), tc.want)
			}
		})
	}
}

func TestSub(t *testing.T) {
	cop := mustCurrency(t, "COP")
	cases := map[string]struct {
		a, b    int64
		want    int64
		wantErr error
	}{
		"positive":        {400, 150, 250, nil},
		"negative result": {150, 400, -250, nil},
		"MinInt64 - 1":    {math.MinInt64, 1, 0, ErrOverflow},
		"MaxInt64 - -1":   {math.MaxInt64, -1, 0, ErrOverflow},
		"0 - MinInt64":    {0, math.MinInt64, 0, ErrOverflow},
		"-1 - MinInt64":   {-1, math.MinInt64, math.MaxInt64, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := New(tc.a, cop).Sub(New(tc.b, cop))

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if err == nil && got != New(tc.want, cop) {
				t.Fatalf("Sub = %d %s, want %d COP", got.Units(), got.Currency().Code(), tc.want)
			}
		})
	}
}

func TestDifferentCurrenciesNeverMix(t *testing.T) {
	cop, usd := mustCurrency(t, "COP"), mustCurrency(t, "USD")
	ops := map[string]func(a, b Amount) (Amount, error){"Add": Amount.Add, "Sub": Amount.Sub}
	for name, op := range ops {
		if _, err := op(New(100, cop), New(100, usd)); !errors.Is(err, ErrCurrencyMismatch) {
			t.Errorf("%s(COP, USD) error = %v, want ErrCurrencyMismatch", name, err)
		}
	}
}

// TestAmountWithoutCurrencyNeverAdds checks that the zero value (what is left after a discarded error)
// does not accumulate silently.
func TestAmountWithoutCurrencyNeverAdds(t *testing.T) {
	for name, op := range map[string]func(a, b Amount) (Amount, error){"Add": Amount.Add, "Sub": Amount.Sub} {
		if _, err := op(Amount{}, Amount{}); !errors.Is(err, ErrCurrencyMismatch) {
			t.Errorf("%s(Amount{}, Amount{}) error = %v, want ErrCurrencyMismatch", name, err)
		}
	}
}

func TestNeg(t *testing.T) {
	cop := mustCurrency(t, "COP")

	got, err := New(250, cop).Neg()
	if err != nil || got != New(-250, cop) {
		t.Fatalf("Neg(250) = %d, %v", got.Units(), err)
	}
	if _, err := New(math.MinInt64, cop).Neg(); !errors.Is(err, ErrOverflow) {
		t.Fatalf("Neg(MinInt64) error = %v, want ErrOverflow", err)
	}
}

func TestSignAndIsZero(t *testing.T) {
	cop := mustCurrency(t, "COP")
	cases := []struct {
		units int64
		sign  int
	}{{-7, -1}, {0, 0}, {7, 1}}
	for _, tc := range cases {
		a := New(tc.units, cop)
		if a.Sign() != tc.sign || a.IsZero() != (tc.sign == 0) {
			t.Errorf("%d: Sign = %d, IsZero = %v", tc.units, a.Sign(), a.IsZero())
		}
	}
}
