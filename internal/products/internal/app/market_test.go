package app

import (
	"errors"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/money"
)

func marketValue(t *testing.T, s string) money.Precise {
	t.Helper()
	v, err := money.ParsePrecise(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestTheShapeOfAValue: a loaded value is digits with an optional point, without sign or exponent.
func TestTheShapeOfAValue(t *testing.T) {
	for _, text := range []string{"1e3", "+1", "-1", ".5", "1.", "1,5", "", " 1", "0x1"} {
		if _, err := ParseMarketValue(text); !errors.Is(err, ErrInvalidMarketParameter) {
			t.Errorf("%q = %v, want ErrInvalidMarketParameter", text, err)
		}
	}
	for _, text := range []string{"0", "49799", "0.25316789", "1.000"} {
		if _, err := ParseMarketValue(text); err != nil {
			t.Errorf("%q = %v, want it read", text, err)
		}
	}
}

// TestAParameterIsChecked: the kind, the modality only for the rates, a period that ends after it starts,
// a positive value with up to 8 decimals and, for a rate, at most 1.
func TestAParameterIsChecked(t *testing.T) {
	jan1, feb1 := calendar.Date{Year: 2026, Month: 1, Day: 1}, calendar.Date{Year: 2026, Month: 2, Day: 1}
	p := func(kind MarketKind, modality, value string) MarketParameter {
		return MarketParameter{Kind: kind, Modality: modality, ValidFrom: jan1, ValidTo: feb1, Value: marketValue(t, value)}
	}
	invalid := map[string]MarketParameter{
		"unknown kind":           p("inflation", "", "0.05"),
		"usury without modality": p(UsuryRate, "", "0.25"),
		"UVT with modality":      p(UVT, "annual", "49799"),
		"modality in capitals":   p(IBR, "Overnight", "0.09"),
		"backwards period":       {Kind: UVT, ValidFrom: feb1, ValidTo: jan1, Value: marketValue(t, "49799")},
		"zero":                   p(SMMLV, "", "0"),
		"negative":               p(SMMLV, "", "-1"),
		"rate above 1":           p(UsuryRate, "consumer", "1.0001"),
		"9 decimals":             p(IBR, "overnight", "0.092500001"),
	}
	for name, mp := range invalid {
		if err := validMarketParameter(mp); !errors.Is(err, ErrInvalidMarketParameter) {
			t.Errorf("%s = %v, want ErrInvalidMarketParameter", name, err)
		}
	}
	for name, mp := range map[string]MarketParameter{
		"rate of 1": p(UsuryRate, "consumer", "1.000"), "UVT": p(UVT, "", "49799"), "IBR": p(IBR, "overnight", "0.0925"),
	} {
		if err := validMarketParameter(mp); err != nil {
			t.Errorf("%s = %v, want it valid", name, err)
		}
	}
}

// TestDatesAreOrdered: before compares year, month and day in that order.
func TestDatesAreOrdered(t *testing.T) {
	d := func(y, m, day int) calendar.Date { return calendar.Date{Year: y, Month: time.Month(m), Day: day} }
	cases := []struct {
		a, b calendar.Date
		want bool
	}{
		{d(2026, 1, 31), d(2026, 2, 1), true},
		{d(2025, 12, 31), d(2026, 1, 1), true},
		{d(2026, 2, 1), d(2026, 1, 31), false},
		{d(2026, 1, 1), d(2026, 1, 1), false},
	}
	for _, c := range cases {
		if got := before(c.a, c.b); got != c.want {
			t.Errorf("before(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
