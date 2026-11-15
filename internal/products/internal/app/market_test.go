package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
		"SMMLV with cents":       p(SMMLV, "", "1423.5"),
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
		"SMMLV with zero cents": p(SMMLV, "", "1423500.00"),
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

// wageSeries is an SMMLV series by the day a value holds on; it records the series and days it was asked for, and
// fails with err if set.
type wageSeries struct {
	values map[calendar.Date]string
	err    error
	asked  []string
}

func (w *wageSeries) InsertParameter(context.Context, MarketParameter) error  { return nil }
func (w *wageSeries) Overlaps(context.Context, MarketParameter) (bool, error) { return false, nil }

func (w *wageSeries) ValueOn(_ context.Context, kind MarketKind, modality string, on calendar.Date) (money.Precise, error) {
	w.asked = append(w.asked, fmt.Sprintf("%s %q %s", kind, modality, on))
	if w.err != nil {
		return money.Precise{}, w.err
	}
	v, ok := w.values[on]
	if !ok {
		return money.Precise{}, ErrNoMarketParameter
	}
	return money.ParsePrecise(v)
}

// TestAnSMMLVFarFromItsNeighboursIsRefused (Q-111 i): an SMMLV for 2026 is more than half and less than double the
// one in force on 2025-12-31 and the one on 2027-01-01, if there are, when checked and when loaded; a store error is
// not taken for a missing neighbour, and other kinds are not checked so.
func TestAnSMMLVFarFromItsNeighboursIsRefused(t *testing.T) {
	dec31, jan1, jan1Next := calendar.Date{Year: 2025, Month: 12, Day: 31}, calendar.Date{Year: 2026, Month: 1, Day: 1}, calendar.Date{Year: 2027, Month: 1, Day: 1}
	p := func(kind MarketKind, value string) MarketParameter {
		return MarketParameter{Kind: kind, ValidFrom: jan1, ValidTo: jan1Next, Value: marketValue(t, value)}
	}
	before := &wageSeries{values: map[calendar.Date]string{dec31: "1423500"}}
	after := &wageSeries{values: map[calendar.Date]string{jan1Next: "1750905"}}
	cases := []struct {
		series *wageSeries
		value  string
		want   error
	}{
		{&wageSeries{}, "142350", nil},
		{before, "1423500", nil},
		{before, "1000000", nil},
		{before, "711750", ErrInvalidMarketParameter},
		{before, "711751", nil},
		{before, "2847000", ErrInvalidMarketParameter},
		{before, "2846999", nil},
		{before, "14235000", ErrInvalidMarketParameter},
		{after, "14235000", ErrInvalidMarketParameter},
		{after, "1423500", nil},
	}
	for _, c := range cases {
		if err := CheckMarketParameter(t.Context(), c.series, p(SMMLV, c.value)); !errors.Is(err, c.want) {
			t.Errorf("checking an SMMLV of %s near %v = %v, want %v", c.value, c.series.values, err, c.want)
		}
		if err := LoadMarketParameter(t.Context(), c.series, p(SMMLV, c.value)); !errors.Is(err, c.want) {
			t.Errorf("loading an SMMLV of %s near %v = %v, want %v", c.value, c.series.values, err, c.want)
		}
	}
	if want := []string{`smmlv "" 2025-12-31`, `smmlv "" 2027-01-01`}; !slices.Equal(after.asked[:2], want) {
		t.Errorf("the neighbours read = %q, want %q", after.asked[:2], want)
	}
	boom := errors.New("boom")
	if err := CheckMarketParameter(t.Context(), &wageSeries{err: boom}, p(SMMLV, "1423500")); !errors.Is(err, boom) {
		t.Errorf("with the store failing = %v, want its error", err)
	}
	if err := CheckMarketParameter(t.Context(), before, p(UVT, "49799")); err != nil {
		t.Errorf("a UVT of 49799 near an SMMLV of 1423500 = %v, want it valid", err)
	}
}
