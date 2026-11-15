package httpapi

import (
	"math"
	"testing"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
)

// TestAManualPostingReachesItsThreshold (ADR-0019): what counts is the total of its debits in a currency the
// threshold names, which saturates instead of wrapping around.
func TestAManualPostingReachesItsThreshold(t *testing.T) {
	cop, usd := currency(t, "COP"), currency(t, "USD")
	entry := func(units int64, c money.Currency) ledger.EntryInput {
		return ledger.EntryInput{AccountID: ids.New(), Amount: money.New(units, c)}
	}
	thresholds := ManualPostingThresholds{"COP": 100}
	for name, c := range map[string]struct {
		entries []ledger.EntryInput
		want    bool
	}{
		"its debits reach it":             {[]ledger.EntryInput{entry(60, cop), entry(40, cop), entry(-100, cop)}, true},
		"its debits stay below":           {[]ledger.EntryInput{entry(99, cop), entry(-99, cop)}, false},
		"a currency it does not name":     {[]ledger.EntryInput{entry(1000, usd), entry(-1000, usd)}, false},
		"balanced debits that would wrap": {[]ledger.EntryInput{entry(math.MaxInt64, cop), entry(-math.MaxInt64, cop), entry(math.MaxInt64, cop), entry(-math.MaxInt64, cop)}, true},
		"one currency of several reaches": {[]ledger.EntryInput{entry(5, usd), entry(-5, usd), entry(100, cop), entry(-100, cop)}, true},
	} {
		if got := thresholds.reached(c.entries); got != c.want {
			t.Errorf("%s: reached = %v, want %v", name, got, c.want)
		}
	}
	if (ManualPostingThresholds{}).reached([]ledger.EntryInput{entry(math.MaxInt64, cop)}) {
		t.Error("with no threshold, a posting reached one")
	}
}

func currency(t *testing.T, code string) money.Currency {
	t.Helper()
	c, err := money.CurrencyOf(code)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
