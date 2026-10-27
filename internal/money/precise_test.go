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

// fataler es lo que comparten *testing.T y *rapid.T.
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
			t.Errorf("ToAmount(%s, modo %d) = %v, %v; se esperaba %d JPY", tc.in, tc.mode, got, err, tc.want)
		}
	}
}

// TestToAmountUsesMinorUnits: Precise está en unidades mayores; el monto, en menores de su moneda.
func TestToAmountUsesMinorUnits(t *testing.T) {
	cop := mustCurrency(t, "COP")
	got, residue, err := mustPrecise(t, "100000.005").ToAmount(cop, HalfEven)
	if err != nil || got != New(10_000_000, cop) {
		t.Fatalf("monto = %v, %v; se esperaban 10.000.000 unidades", got, err)
	}
	if residue.Cmp(mustPrecise(t, "0.005")) != 0 {
		t.Errorf("residuo = %v, se esperaba 0,005", residue)
	}
	if back := New(10_000_000, cop).Precise(); back.Cmp(mustPrecise(t, "100000")) != 0 {
		t.Errorf("Precise() = %v, se esperaba 100000", back)
	}
}

// TestToAmountResidueOfTheSmallestValue: el residuo de un valor con 100 decimales es exacto.
func TestToAmountResidueOfTheSmallestValue(t *testing.T) {
	tiny := "1." + strings.Repeat("0", 99) + "1"
	got, residue, err := mustPrecise(t, tiny).ToAmount(mustCurrency(t, "COP"), Down)
	if err != nil || got.Units() != 100 || residue.Cmp(mustPrecise(t, "1e-100")) != 0 {
		t.Fatalf("= %v, residuo %v, %v; se esperaban 100 unidades y residuo 1e-100", got, residue, err)
	}
}

func TestToAmountErrors(t *testing.T) {
	cop := mustCurrency(t, "COP")
	if _, _, err := mustPrecise(t, "92233720368547758.08").ToAmount(cop, HalfEven); !errors.Is(err, ErrOverflow) {
		t.Errorf("un valor que no cabe en int64 = %v, se esperaba ErrOverflow", err)
	}
	if _, _, err := mustPrecise(t, "1").ToAmount(cop, RoundingMode(99)); !errors.Is(err, ErrInvalidRounding) {
		t.Errorf("un modo de redondeo inexistente = %v, se esperaba ErrInvalidRounding", err)
	}
	if _, _, err := mustPrecise(t, "1").ToAmount(Currency{}, HalfEven); !errors.Is(err, ErrUnknownCurrency) {
		t.Errorf("sin moneda = %v, se esperaba ErrUnknownCurrency", err)
	}
}

func TestParsePreciseRejects(t *testing.T) {
	invalid := []string{
		"", "abc", "NaN", "Infinity", "-inf", "1,5",
		"1e101", "1e999999999", "1e-101", "1." + strings.Repeat("0", 100) + "1", // fuera de ±10^100 o con más de 100 decimales
	}
	for _, s := range invalid {
		if _, err := ParsePrecise(s); !errors.Is(err, ErrInvalidDecimal) {
			t.Errorf("ParsePrecise(%.20q) = %v, se esperaba ErrInvalidDecimal", s, err)
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
		{"0,1 + 0,2", Precise.Add, "0.1", "0.2", "0.3", nil},
		{"100000 × 0,004", Precise.Mul, "100000", "0.004", "400", nil},
		{"1 / 3 a 4 decimales", quo(4, HalfEven), "1", "3", "0.3333", nil},
		{"2 / 3 truncado a 2 decimales", quo(2, Down), "2", "3", "0.66", nil},
		{"-2 / 3 truncado a 2 decimales", quo(2, Down), "-2", "3", "-0.66", nil},
		{"1 / -8 a 2 decimales, empate al par", quo(2, HalfEven), "1", "-8", "-0.12", nil},
		{"un solo redondeo: 0,99…9 truncado no sube a 1", quo(2, Down), "0.99999999999999999999999999999999999999", "1", "0.99", nil},
		{"1 / 0", quo(2, HalfEven), "1", "0", "", ErrDivisionByZero},
		{"escala negativa", quo(-1, HalfEven), "1", "1", "", ErrInvalidDecimal},
		{"1e100 / 1e-100 se sale del rango", quo(0, HalfEven), "1e100", "1e-100", "", ErrOverflow},
		{"1e100 × 10 se sale del rango", Precise.Mul, "1e100", "10", "", ErrOverflow},
		{"1e-60 × 1e-60 se sale del rango", Precise.Mul, "1e-60", "1e-60", "", ErrOverflow},
		{"un producto con más de 100 decimales", Precise.Mul, sixtyDecimals, sixtyDecimals, "", ErrOverflow},
		{"raíz de un negativo", Precise.Pow, "-1", "0.5", "", ErrInvalidDecimal},
		{"0 elevado a un negativo no es infinito", Precise.Pow, "0", "-1", "", ErrDivisionByZero},
	}
	for _, tc := range cases {
		got, err := tc.op(mustPrecise(t, tc.a), mustPrecise(t, tc.b))
		if tc.wantErr != nil {
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("%s: error = %v, se esperaba %v", tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got.Cmp(mustPrecise(t, tc.want)) != 0 {
			t.Errorf("%s = %v, %v; se esperaba %s", tc.name, got, err, tc.want)
		}
	}
}

// TestPreciseDailyRate: tasa efectiva diaria de un 12 % E.A., (1,12)^(1/365).
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
		t.Errorf("1,12^(1/365) = %v, %v; se esperaba 1,0003105378", rounded, err)
	}
}

func TestPreciseCmpAndZero(t *testing.T) {
	if mustPrecise(t, "1.50").Cmp(mustPrecise(t, "1.5")) != 0 {
		t.Error("1,50 y 1,5 deben ser iguales")
	}
	if mustPrecise(t, "-1").Cmp(mustPrecise(t, "1")) != -1 {
		t.Error("-1 debe ser menor que 1")
	}
	var zero Precise
	if zero.Cmp(mustPrecise(t, "0")) != 0 || zero.String() != "0" {
		t.Errorf("el valor cero de Precise es 0, no %v", zero)
	}
	if parsed := mustPrecise(t, "-0"); parsed.String() != "0" {
		t.Errorf(`ParsePrecise("-0") = %v, se esperaba 0 sin signo`, parsed)
	}
	if negZero, err := mustPrecise(t, "-1").Mul(zero); err != nil || negZero.String() != "0" {
		t.Errorf("-1 × 0 = %v, %v; se esperaba 0 sin signo", negZero, err)
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
		// Las unidades sobrantes van a las de mayor residuo (los pesos 2 tienen 14/60; los 1, 7/60) y,
		// en empate, a las primeras. Son 40 partes porque con 12 o menos slices ordena por inserción,
		// que es estable, y un orden inestable pasaría la prueba.
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
			t.Errorf("Allocate(%d, %v) = %v, %v; se esperaba %v", tc.units, tc.weights, got, err, want)
		}
	}
}

func TestAllocateRejects(t *testing.T) {
	cop := mustCurrency(t, "COP")
	for _, weights := range [][]int64{nil, {}, {0, 0}, {2, -1}} {
		if _, err := New(100, cop).Allocate(weights); !errors.Is(err, ErrInvalidWeights) {
			t.Errorf("Allocate(%v) = %v, se esperaba ErrInvalidWeights", weights, err)
		}
	}
	if _, err := (Amount{units: 100}).Allocate([]int64{1, 1}); !errors.Is(err, ErrUnknownCurrency) {
		t.Errorf("repartir un monto sin moneda = %v, se esperaba ErrUnknownCurrency", err)
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

// checkAllocation verifica que las partes sumen units, que cada una sea el piso de su cuota exacta o
// el piso más uno, y que una unidad sobrante nunca vaya a una parte con menor residuo que otra que no
// la recibió.
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
			t.Fatalf("la parte %d (%d) no es el piso de su cuota (%v) ni el piso más uno", i, a.Units(), floor)
		}
		extra[i], remainders[i] = part.Cmp(floor) > 0, rem
	}
	if sum.Cmp(big.NewInt(units)) != 0 {
		t.Fatalf("Σ Allocate = %v, se esperaba %d", sum, units)
	}
	checkLargestRemainders(t, extra, remainders)
}

// checkLargestRemainders: ninguna parte que recibió una unidad sobrante tiene menor residuo que una
// que no la recibió.
func checkLargestRemainders(t *rapid.T, extra []bool, remainders []*big.Int) {
	for i := range extra {
		for j := range extra {
			if extra[i] && !extra[j] && remainders[i].Cmp(remainders[j]) < 0 {
				t.Fatalf("la parte %d recibió una unidad con menor residuo que la parte %d", i, j)
			}
		}
	}
}

// amountSized: con 13 dígitos enteros y CLF (exponente 4), el monto todavía cabe en int64; con más, la
// propiedad de ToAmount fallaría por overflow y no por un error de redondeo.
var amountSized = rapid.StringMatching(`-?[0-9]{1,13}(\.[0-9]{1,10})?`)

func TestToAmountPlusResidueIsTheOriginal(t *testing.T) {
	cur := rapid.SampledFrom([]Currency{mustCurrency(t, "COP"), mustCurrency(t, "JPY"), mustCurrency(t, "CLF")})
	mode := rapid.SampledFrom([]RoundingMode{HalfEven, HalfUp, Down, Up})
	rapid.Check(t, func(t *rapid.T) {
		c, m := cur.Draw(t, "moneda"), mode.Draw(t, "modo")
		p := mustPrecise(t, amountSized.Draw(t, "valor"))
		a, residue, err := p.ToAmount(c, m)
		if err != nil {
			t.Fatal(err)
		}
		back, err := a.Precise().Add(residue)
		if err != nil || back.Cmp(p) != 0 {
			t.Fatalf("%v + %v = %v (%v), se esperaba %v", a.Precise(), residue, back, err, p)
		}
		checkResidue(t, p, residue, c, m)
	})
}

// checkResidue: el residuo de un redondeo al más cercano no pasa de media unidad menor; el de Down y
// Up, de una unidad sin llegar a ella. Down deja el residuo con el signo de p (truncó hacia cero) y Up,
// con el signo contrario.
func checkResidue(t *rapid.T, p, residue Precise, c Currency, m RoundingMode) {
	t.Helper()
	nearest := m == HalfEven || m == HalfUp
	limit := Precise{d: apd.New(1, -int32(c.exponent))} //nolint:gosec // exponentes ISO de un dígito
	if nearest {
		limit = Precise{d: apd.New(5, -int32(c.exponent)-1)} //nolint:gosec // exponentes ISO de un dígito
	}
	size := Precise{d: new(apd.Decimal).Abs(residue.dec())}
	if exceeds := size.Cmp(limit); exceeds > 0 || (!nearest && exceeds == 0) {
		t.Fatalf("residuo %v de %v con el modo %d supera %v", residue, p, m, limit)
	}
	sign, pSign := residue.Cmp(Precise{}), p.Cmp(Precise{})
	if (m == Down && sign*pSign < 0) || (m == Up && sign*pSign > 0) {
		t.Fatalf("residuo %v de %v con el modo %d tiene el signo equivocado", residue, p, m)
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
