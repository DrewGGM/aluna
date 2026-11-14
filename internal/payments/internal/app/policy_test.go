package app

import (
	"slices"
	"testing"

	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/payments/internal/domain"
	"github.com/DrewGGM/aluna/internal/pricing"
)

// TestAClearedPaymentIsNeverChargedMoreThanItWasQuoted (ADR-0016): a payment that waited for compliance with a
// quote is charged, once cleared, the fees it was quoted if the policy honors quotes, or the lower of those and
// the tariff in force, the tariff on a tie; one that kept no quoted fees, the tariff.
func TestAClearedPaymentIsNeverChargedMoreThanItWasQuoted(t *testing.T) {
	cop, err := money.CurrencyOf("COP")
	if err != nil {
		t.Fatal(err)
	}
	fees := func(code string, units int64) []pricing.Fee {
		if units == 0 {
			return []pricing.Fee{}
		}
		return []pricing.Fee{{Code: code, Amount: money.New(units, cop)}}
	}
	cases := map[string]struct {
		policy                 Policy
		quoted                 bool
		quote, tariff, charged int64
		fromQuote              bool
	}{
		"the tariff went up, lower of":   {Policy{}, true, 200000, 300000, 200000, true},
		"the tariff went down, lower of": {Policy{}, true, 300000, 200000, 200000, false},
		"a tie, lower of":                {Policy{}, true, 300000, 300000, 300000, false},
		"quoted free, lower of":          {Policy{}, true, 0, 300000, 0, true},
		"the tariff went down, honored":  {Policy{HonorQuotes: true}, true, 300000, 200000, 300000, true},
		"no quoted fees kept":            {Policy{HonorQuotes: true}, false, 300000, 200000, 200000, false},
	}
	for name, tc := range cases {
		quoted := fees("quoted", tc.quote) // the fee's code tells where the charged fees came from
		o := Order{Order: domain.Order{Amount: money.New(10000000, cop)}, Quoted: tc.quoted, QuotedFees: quoted}

		got, err := tc.policy.charged(o, fees("tariff", tc.tariff))

		total, terr := pricing.Total(got, cop)
		if err != nil || terr != nil || total.Units() != tc.charged || slices.Equal(got, quoted) != tc.fromQuote {
			t.Errorf("%s: charged %v (%v), want %d from the quote: %t", name, got, err, tc.charged, tc.fromQuote)
		}
	}
}
