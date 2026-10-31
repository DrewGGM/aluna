package money

import (
	"errors"
	"math"
	"math/big"
	"slices"
	"strings"
	"testing"

	"github.com/cockroachdb/apd/v3"
	"pgregory.net/rapid"
)

// fataler is what *testing.T and *rapid.T have in common.
type fataler interface {
	Helper()
	Fatalf(format string, args ...any)
}

func mustPrecise(t fataler, s string) Precise {
	t.Helper()
	p, err := ParsePrecise(s)
	if err != nil {
		t.Fatalf("ParsePrecise(%q): %v", s, err)
	}
	return p
}

func TestToAmountRoundingModes(t *testing.T) {
	jpy := mustCurrency(t, "JPY")
	cases := []struct {
		in   string
		mode RoundingMode
		want int64
	}{
		{"0.5", HalfEven, 0},
		{"1.5", HalfEven, 2},
		{"2.5", HalfEven, 2},
		{"2.5", HalfUp, 3},
		{"-2.5", HalfUp, -3},
		{"2.9", Down, 2},
		{"-2.9", Down, -2},
		{"2.1", Up, 3},
		{"-2.1", Up, -3},
		{"7", HalfEven, 7},
	}
	for _, tc := range cases {
		got, _, err := mustPrecise(t, tc.in).ToAmount(jpy, tc.mode)
		if err != nil || got != New(tc.want, jpy) {
			t.Errorf("ToAmount(%s, mode %d) = %v, %v; want %d JPY", tc.in, tc.mode, got, err, tc.want)
		}
	}
}

// TestToAmountUsesMinorUnits: Precise is in major units; the amount, in its currency's minor units.
func TestToAmountUsesMinorUnits(t *testing.T) {
	cop := mustCurrency(t, "COP")
	got, residue, err := mustPrecise(t, "100000.005").ToAmount(cop, HalfEven)
	if err != nil || got != New(10_000_000, cop) {
		t.Fatalf("amount = %v, %v; want 10,000,000 units", got, err)
	}
	if residue.Cmp(mustPrecise(t, "0.005")) != 0 {
		t.Errorf("residue = %v, want 0.005", residue)
	}
	if back := New(10_000_000, cop).Precise(); back.Cmp(mustPrecise(t, "100000")) != 0 {
		t.Errorf("Precise() = %v, want 100000", back)
	}
}

// TestToAmountResidueOfTheSmallestValue: the residue of a value with 100 decimal places is exact.
func TestToAmountResidueOfTheSmallestValue(t *testing.T) {
	tiny := "1." + strings.Repeat("0", 99) + "1"
	got, residue, err := mustPrecise(t, tiny).ToAmount(mustCurrency(t, "COP"), Down)
	if err != nil || got.Units() != 100 || residue.Cmp(mustPrecise(t, "1e-100")) != 0 {
		t.Fatalf("= %v, residue %v, %v; want 100 units and residue 1e-100", got, residue, err)
	}
}

func TestToAmountErrors(t *testing.T) {
	cop := mustCurrency(t, "COP")
	if _, _, err := mustPrecise(t, "92233720368547758.08").ToAmount(cop, HalfEven); !errors.Is(err, ErrOverflow) {
		t.Errorf("a value that does not fit in int64 = %v, want ErrOverflow", err)
	}
	if _, _, err := mustPrecise(t, "1").ToAmount(cop, RoundingMode(99)); !errors.Is(err, ErrInvalidRounding) {
		t.Errorf("a nonexistent rounding mode = %v, want ErrInvalidRounding", err)
	}
	if _, _, err := mustPrecise(t, "1").ToAmount(Currency{}, HalfEven); !errors.Is(err, ErrUnknownCurrency) {
		t.Errorf("no currency = %v, want ErrUnknownCurrency", err)
	}
}

func TestParsePreciseRejects(t *testing.T) {
	invalid := []string{
		"", "abc", "NaN", "Infinity", "-inf", "1,5",
		"1e101", "1e999999999", "1e-101", "1." + strings.Repeat("0", 100) + "1", // outside ±10^100 or with more than 100 decimal places
	}
	for _, s := range invalid {
		if _, err := ParsePrecise(s); !errors.Is(err, ErrInvalidDecimal) {
			t.Errorf("ParsePrecise(%.20q) = %v, want ErrInvalidDecimal", s, err)
		}
	}
}

func TestPreciseOperations(t *testing.T) {
	quo := func(scale int, m RoundingMode) func(a, b Precise) (Precise, error) {
		return func(a, b Precise) (Precise, error) { return a.Quo(b, scale, m) }
	}
	sixtyDecimals := "1." + strings.Repeat("0", 59) + "1"
	cases := []struct {
		name    string
		op      func(a, b Precise) (Precise, error)
		a, b    string
		want    string
		wantErr error
	}{
		{"0.1 + 0.2", Precise.Add, "0.1", "0.2", "0.3", nil},
		{"100000 × 0.004", Precise.Mul, "100000", "0.004", "400", nil},
		{"1 / 3 to 4 decimal places", quo(4, HalfEven), "1", "3", "0.3333", nil},
		{"2 / 3 truncated to 2 decimal places", quo(2, Down), "2", "3", "0.66", nil},
		{"-2 / 3 truncated to 2 decimal places", quo(2, Down), "-2", "3", "-0.66", nil},
		{"1 / -8 to 2 decimal places, tie to even", quo(2, HalfEven), "1", "-8", "-0.12", nil},
		{"a single rounding: truncated 0.99…9 does not go up to 1", quo(2, Down), "0.99999999999999999999999999999999999999", "1", "0.99", nil},
		{"1 / 0", quo(2, HalfEven), "1", "0", "", ErrDivisionByZero},
		{"negative scale", quo(-1, HalfEven), "1", "1", "", ErrInvalidDecimal},
		{"1e100 / 1e-100 is out of range", quo(0, HalfEven), "1e100", "1e-100", "", ErrOverflow},
		{"1e100 × 10 is out of range", Precise.Mul, "1e100", "10", "", ErrOverflow},
		{"1e-60 × 1e-60 is out of range", Precise.Mul, "1e-60", "1e-60", "", ErrOverflow},
		{"a product with more than 100 decimal places", Precise.Mul, sixtyDecimals, sixtyDecimals, "", ErrOverflow},
		{"root of a negative", Precise.Pow, "-1", "0.5", "", ErrInvalidDecimal},
		{"0 raised to a negative is not infinity", Precise.Pow, "0", "-1", "", ErrDivisionByZero},
	}
	for _, tc := range cases {
		got, err := tc.op(mustPrecise(t, tc.a), mustPrecise(t, tc.b))
		if tc.wantErr != nil {
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("%s: error = %v, want %v", tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got.Cmp(mustPrecise(t, tc.want)) != 0 {
			t.Errorf("%s = %v, %v; want %s", tc.name, got, err, tc.want)
		}
	}
}

// TestPreciseDailyRate: daily effective rate of a 12 % effective annual rate, (1.12)^(1/365).
func TestPreciseDailyRate(t *testing.T) {
	exponent, err := mustPrecise(t, "1").Quo(mustPrecise(t, "365"), 20, HalfEven)
	if err != nil {
		t.Fatal(err)
	}
	daily, err := mustPrecise(t, "1.12").Pow(exponent)
	if err != nil {
		t.Fatal(err)
	}
	rounded, err := daily.Quo(mustPrecise(t, "1"), 10, HalfEven)
	if err != nil || rounded.Cmp(mustPrecise(t, "1.0003105378")) != 0 {
		t.Errorf("1.12^(1/365) = %v, %v; want 1.0003105378", rounded, err)
	}
}

func TestPreciseCmpAndZero(t *testing.T) {
	if mustPrecise(t, "1.50").Cmp(mustPrecise(t, "1.5")) != 0 {
		t.Error("1.50 and 1.5 must be equal")
	}
	if mustPrecise(t, "-1").Cmp(mustPrecise(t, "1")) != -1 {
		t.Error("-1 must be less than 1")
	}
	var zero Precise
	if zero.Cmp(mustPrecise(t, "0")) != 0 || zero.String() != "0" {
		t.Errorf("the zero value of Precise is 0, not %v", zero)
	}
	if parsed := mustPrecise(t, "-0"); parsed.String() != "0" {
		t.Errorf(`ParsePrecise("-0") = %v, want unsigned 0`, parsed)
	}
	if negZero, err := mustPrecise(t, "-1").Mul(zero); err != nil || negZero.String() != "0" {
		t.Errorf("-1 × 0 = %v, %v; want unsigned 0", negZero, err)
	}
}

func TestAllocateFixedCases(t *testing.T) {
	cop := mustCurrency(t, "COP")
	cases := []struct {
		units   int64
		weights []int64
		want    []int64
	}{
		{100, []int64{1, 1, 1}, []int64{34, 33, 33}},
		{-100, []int64{1, 1, 1}, []int64{-34, -33, -33}},
		{5, []int64{1, 0, 1}, []int64{3, 0, 2}},
		{10, []int64{3, 7}, []int64{3, 7}},
		{1, []int64{1, 1}, []int64{1, 0}},
		// The leftover units go to the parts with the largest remainder (weights of 2 have 14/60; weights
		// of 1, 7/60) and, on a tie, to the first ones. There are 40 parts because with 12 or fewer slices
		// sorts by insertion, which is stable, and an unstable sort would pass the test.
		{7, slices.Repeat([]int64{1, 2}, 20), slices.Concat(slices.Repeat([]int64{0, 1}, 7), make([]int64, 26))},
		{math.MaxInt64, []int64{math.MaxInt64, math.MaxInt64}, []int64{math.MaxInt64/2 + 1, math.MaxInt64 / 2}},
		{math.MinInt64, []int64{1}, []int64{math.MinInt64}},
	}
	for _, tc := range cases {
		got, err := New(tc.units, cop).Allocate(tc.weights)
		want := make([]Amount, len(tc.want))
		for i, u := range tc.want {
			want[i] = New(u, cop)
		}
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("Allocate(%d, %v) = %v, %v; want %v", tc.units, tc.weights, got, err, want)
		}
	}
}

func TestAllocateRejects(t *testing.T) {
	cop := mustCurrency(t, "COP")
	for _, weights := range [][]int64{nil, {}, {0, 0}, {2, -1}} {
		if _, err := New(100, cop).Allocate(weights); !errors.Is(err, ErrInvalidWeights) {
			t.Errorf("Allocate(%v) = %v, want ErrInvalidWeights", weights, err)
		}
	}
	if _, err := (Amount{units: 100}).Allocate([]int64{1, 1}); !errors.Is(err, ErrUnknownCurrency) {
		t.Errorf("allocate an amount with no currency = %v, want ErrUnknownCurrency", err)
	}
}

func TestAllocatePreservesTotalAndFollowsTheLargestRemainders(t *testing.T) {
	cop := mustCurrency(t, "COP")
	rapid.Check(t, func(t *rapid.T) {
		units := rapid.Int64().Draw(t, "units")
		weights := rapid.SliceOfN(rapid.Int64Range(0, math.MaxInt64), 1, 20).Draw(t, "weights")
		if slices.Max(weights) == 0 {
			weights[0] = 1
		}
		got, err := New(units, cop).Allocate(weights)
		if err != nil {
			t.Fatal(err)
		}
		checkAllocation(t, units, weights, got)
	})
}

// checkAllocation checks that the parts add up to units, that each one is the floor of its exact quota
// or the floor plus one, and that a leftover unit never goes to a part with a smaller remainder than
// another that did not get one.
func checkAllocation(t *rapid.T, units int64, weights []int64, got []Amount) {
	total, sum := big.NewInt(0), big.NewInt(0)
	for _, w := range weights {
		total.Add(total, big.NewInt(w))
	}
	whole := new(big.Int).Abs(big.NewInt(units))
	extra, remainders := make([]bool, len(got)), make([]*big.Int, len(got))
	for i, a := range got {
		sum.Add(sum, big.NewInt(a.Units()))
		floor, rem := new(big.Int).QuoRem(new(big.Int).Mul(whole, big.NewInt(weights[i])), total, new(big.Int))
		part := new(big.Int).Abs(big.NewInt(a.Units()))
		if d := new(big.Int).Sub(part, floor); d.Sign() < 0 || d.Cmp(big.NewInt(1)) > 0 {
			t.Fatalf("part %d (%d) is neither the floor of its quota (%v) nor the floor plus one", i, a.Units(), floor)
		}
		extra[i], remainders[i] = part.Cmp(floor) > 0, rem
	}
	if sum.Cmp(big.NewInt(units)) != 0 {
		t.Fatalf("Σ Allocate = %v, want %d", sum, units)
	}
	checkLargestRemainders(t, extra, remainders)
}

// checkLargestRemainders: no part that got a leftover unit has a smaller remainder than one that did
// not.
func checkLargestRemainders(t *rapid.T, extra []bool, remainders []*big.Int) {
	for i := range extra {
		for j := range extra {
			if extra[i] && !extra[j] && remainders[i].Cmp(remainders[j]) < 0 {
				t.Fatalf("part %d got a unit with a smaller remainder than part %d", i, j)
			}
		}
	}
}

// amountSized: with 13 integer digits and CLF (exponent 4), the amount still fits in int64; with more,
// the ToAmount property would fail on overflow and not on a rounding error.
var amountSized = rapid.StringMatching(`-?[0-9]{1,13}(\.[0-9]{1,10})?`)

func TestToAmountPlusResidueIsTheOriginal(t *testing.T) {
	cur := rapid.SampledFrom([]Currency{mustCurrency(t, "COP"), mustCurrency(t, "JPY"), mustCurrency(t, "CLF")})
	mode := rapid.SampledFrom([]RoundingMode{HalfEven, HalfUp, Down, Up})
	rapid.Check(t, func(t *rapid.T) {
		c, m := cur.Draw(t, "currency"), mode.Draw(t, "mode")
		p := mustPrecise(t, amountSized.Draw(t, "value"))
		a, residue, err := p.ToAmount(c, m)
		if err != nil {
			t.Fatal(err)
		}
		back, err := a.Precise().Add(residue)
		if err != nil || back.Cmp(p) != 0 {
			t.Fatalf("%v + %v = %v (%v), want %v", a.Precise(), residue, back, err, p)
		}
		checkResidue(t, p, residue, c, m)
	})
}

// checkResidue: the residue of a round-to-nearest does not exceed half a minor unit; that of Down and
// Up stays below one unit. Down leaves the residue with p's sign (it truncated toward zero) and Up, with
// the opposite sign.
func checkResidue(t *rapid.T, p, residue Precise, c Currency, m RoundingMode) {
	t.Helper()
	nearest := m == HalfEven || m == HalfUp
	limit := Precise{d: apd.New(1, -int32(c.exponent))} //nolint:gosec // one-digit ISO exponents
	if nearest {
		limit = Precise{d: apd.New(5, -int32(c.exponent)-1)} //nolint:gosec // one-digit ISO exponents
	}
	size := Precise{d: new(apd.Decimal).Abs(residue.dec())}
	if exceeds := size.Cmp(limit); exceeds > 0 || (!nearest && exceeds == 0) {
		t.Fatalf("residue %v of %v with mode %d exceeds %v", residue, p, m, limit)
	}
	sign, pSign := residue.Cmp(Precise{}), p.Cmp(Precise{})
	if (m == Down && sign*pSign < 0) || (m == Up && sign*pSign > 0) {
		t.Fatalf("residue %v of %v with mode %d has the wrong sign", residue, p, m)
	}
}

func TestPreciseAddIsAssociative(t *testing.T) {
	anyDecimal := rapid.StringMatching(`-?[0-9]{1,20}(\.[0-9]{1,10})?`)
	rapid.Check(t, func(t *rapid.T) {
		a := mustPrecise(t, anyDecimal.Draw(t, "a"))
		b := mustPrecise(t, anyDecimal.Draw(t, "b"))
		c := mustPrecise(t, anyDecimal.Draw(t, "c"))
		ab, err1 := a.Add(b)
		left, err2 := ab.Add(c)
		bc, err3 := b.Add(c)
		right, err4 := a.Add(bc)
		if err := errors.Join(err1, err2, err3, err4); err != nil {
			t.Fatal(err)
		}
		if left.Cmp(right) != 0 {
			t.Fatalf("(a+b)+c = %v ≠ a+(b+c) = %v", left, right)
		}
	})
}
