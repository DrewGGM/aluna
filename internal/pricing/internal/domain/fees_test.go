package domain

import (
	"slices"
	"testing"

	"github.com/DrewGGM/aluna/internal/money"
)

func cop(t *testing.T) money.Currency {
	t.Helper()
	c, err := money.CurrencyOf("COP")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// rules are the fees of the tiered savings product of products' testdata: a withdrawal fee that the first
// withdrawals of the month do not pay, and a transfer fee with no exemption.
var rules = []FeeRule{
	{Code: "withdrawal_fee", Operation: Withdrawal, AmountUnits: 450000, ExemptRule: "first_withdrawals_free"},
	{Code: "transfer_fee", Operation: Transfer, AmountUnits: 300000},
}

// TestEachOperationPaysItsFees (FR-28): an operation pays the fees whose trigger it is, in the product's order,
// in the account's currency, unless the fee's exemption rule holds; a fee without a rule is always paid.
func TestEachOperationPaysItsFees(t *testing.T) {
	c := cop(t)
	never := func(string) bool { return false }
	always := func(string) bool { return true }
	for name, tc := range map[string]struct {
		operation Operation
		exempt    func(string) bool
		want      []Fee
	}{
		"a withdrawal":                 {Withdrawal, never, []Fee{{Code: "withdrawal_fee", Amount: money.New(450000, c)}}},
		"an exempt withdrawal":         {Withdrawal, always, nil},
		"a transfer":                   {Transfer, never, []Fee{{Code: "transfer_fee", Amount: money.New(300000, c)}}},
		"a transfer, its fee has none": {Transfer, always, []Fee{{Code: "transfer_fee", Amount: money.New(300000, c)}}},
	} {
		if got := FeesFor(rules, tc.operation, c, tc.exempt); !slices.Equal(got, tc.want) {
			t.Errorf("%s: fees = %+v, want %+v", name, got, tc.want)
		}
	}
}

// TestAnExemptionIsAskedByItsName: the exemption is asked for the rule the fee names, and only for a fee of the
// operation.
func TestAnExemptionIsAskedByItsName(t *testing.T) {
	var asked []string
	FeesFor(rules, Withdrawal, cop(t), func(rule string) bool {
		asked = append(asked, rule)
		return false
	})
	if !slices.Equal(asked, []string{"first_withdrawals_free"}) {
		t.Fatalf("asked %v, want only first_withdrawals_free", asked)
	}
}

// TestFeesAddUp: the total of a quote is its fees added, in the account's currency.
func TestFeesAddUp(t *testing.T) {
	c := cop(t)
	total, err := Total([]Fee{{Code: "a", Amount: money.New(100, c)}, {Code: "b", Amount: money.New(250, c)}}, c)
	if err != nil || total != money.New(350, c) {
		t.Fatalf("total = %v (%v), want 350", total, err)
	}
	if none, err := Total(nil, c); err != nil || none != money.New(0, c) {
		t.Fatalf("total of no fees = %v (%v), want 0", none, err)
	}
}
