package domain

import (
	"testing"

	"pgregory.net/rapid"

	"github.com/DrewGGM/aluna/internal/money"
)

// TestAGMFIsFourPerThousandRoundedToThePeso (FR-29): what the exemption does not cover pays 4 per thousand,
// rounded half up to the peso; the exemption covers what remains of it, up to the whole debit.
func TestAGMFIsFourPerThousandRoundedToThePeso(t *testing.T) {
	c := cop(t)
	pesos := func(units int64) money.Amount { return money.New(units*100, c) }
	none := money.New(0, c)
	for name, tc := range map[string]struct {
		debit, remaining money.Amount
		want             GMF
	}{
		"a million":                  {pesos(1000000), none, GMF{Exempt: none, Tax: pesos(4000)}},
		"half a peso goes up":        {pesos(125), none, GMF{Exempt: none, Tax: pesos(1)}},
		"two and a half go up":       {pesos(625), none, GMF{Exempt: none, Tax: pesos(3)}},
		"less than half a peso":      {money.New(12499, c), none, GMF{Exempt: none, Tax: none}},
		"partly exempt":              {pesos(1000000), pesos(400000), GMF{Exempt: pesos(400000), Tax: pesos(2400)}},
		"wholly exempt":              {pesos(1000000), pesos(5000000), GMF{Exempt: pesos(1000000), Tax: none}},
		"exactly what remains":       {pesos(1000000), pesos(1000000), GMF{Exempt: pesos(1000000), Tax: none}},
		"cents that do not round up": {money.New(100000050, c), none, GMF{Exempt: none, Tax: pesos(4000)}},
	} {
		got, err := GMFOf(tc.debit, tc.remaining)
		if err != nil || got != tc.want {
			t.Errorf("%s: GMF = %+v (%v), want %+v", name, got, err, tc.want)
		}
	}
}

// TestOnlyDebitsInPesosPayGMF: a debit in another currency pays no GMF and uses no exemption.
func TestOnlyDebitsInPesosPayGMF(t *testing.T) {
	usd, err := money.CurrencyOf("USD")
	if err != nil {
		t.Fatal(err)
	}
	got, err := GMFOf(money.New(100000000, usd), money.New(0, cop(t)))
	if want := (GMF{Exempt: money.New(0, usd), Tax: money.New(0, usd)}); err != nil || got != want {
		t.Fatalf("GMF of dollars = %+v (%v), want %+v", got, err, want)
	}
}

// TestTheExemptionIs350UVT (FR-29): the cap is 350 UVT in pesos, truncated to the cent.
func TestTheExemptionIs350UVT(t *testing.T) {
	for uvt, want := range map[string]int64{
		"49799":  1742965000, // 2025: 17,429,650 COP
		"1.0001": 35003,      // 350.035 COP: truncated, not rounded
	} {
		v, err := money.ParsePrecise(uvt)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ExemptionCap(v)
		if err != nil || got != money.New(want, cop(t)) {
			t.Errorf("cap of a UVT of %s = %v (%v), want %d units of COP", uvt, got, err, want)
		}
	}
}

// TestAMonthNeverExemptsMoreThanItsCap (FR-29): whatever the debits of a month, applying each with what
// remains of the cap never exempts more than the cap; each debit is its exempt part plus the part that pays,
// and that part pays its 4 per thousand.
func TestAMonthNeverExemptsMoreThanItsCap(t *testing.T) {
	c := cop(t)
	rapid.Check(t, func(rt *rapid.T) {
		limit := money.New(rapid.Int64Range(0, 1833102950).Draw(rt, "cap"), c)
		used := money.New(0, c)
		for i := range rapid.IntRange(1, 30).Draw(rt, "debits") {
			debit := money.New(rapid.Int64Range(1, 1000000000).Draw(rt, "debit"), c)
			remaining, err := limit.Sub(used)
			if err != nil {
				rt.Fatal(err)
			}
			g, err := GMFOf(debit, remaining)
			if err != nil {
				rt.Fatal(err)
			}
			if used, err = used.Add(g.Exempt); err != nil {
				rt.Fatal(err)
			}
			taxed := debit.Units() - g.Exempt.Units()
			if used.Units() > limit.Units() || g.Exempt.Sign() < 0 || taxed < 0 || g.Tax.Units() != (taxed*4+50000)/100000*100 {
				rt.Fatalf("debit %d: %d of %v exempt, %+v; used %v of %v", i, debit.Units(), debit, g, used, limit)
			}
		}
	})
}
