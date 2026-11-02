package postingrules

import (
	"errors"
	"math"
	"slices"
	"testing"

	"pgregory.net/rapid"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
)

func cop(t *testing.T, units int64) money.Amount {
	t.Helper()
	c, err := money.CurrencyOf("COP")
	if err != nil {
		t.Fatal(err)
	}
	return money.New(units, c)
}

// TestEachChargeDebitsTheCustomer: a fee and the GMF each debit the customer's account and credit their own
// account, in the order given.
func TestEachChargeDebitsTheCustomer(t *testing.T) {
	customer, income, payable := ids.New(), ids.New(), ids.New()

	entries, err := Charges(customer, []Charge{{Amount: cop(t, 500000), Counter: income}, {Amount: cop(t, 120000), Counter: payable}})
	if err != nil {
		t.Fatal(err)
	}

	want := []ledger.EntryInput{
		{AccountID: customer, Amount: cop(t, 500000)},
		{AccountID: income, Amount: cop(t, -500000)},
		{AccountID: customer, Amount: cop(t, 120000)},
		{AccountID: payable, Amount: cop(t, -120000)},
	}
	if !slices.Equal(entries, want) {
		t.Fatalf("entries = %+v, want %+v", entries, want)
	}
}

// TestNoChargesAreNoEntries: a payment without fees or GMF posts nothing besides its capture.
func TestNoChargesAreNoEntries(t *testing.T) {
	entries, err := Charges(ids.New(), nil)
	if err != nil || len(entries) != 0 {
		t.Fatalf("Charges(none) = %+v, %v; want no entries", entries, err)
	}
}

// TestAChargeTheRulesCannotPost: an amount that is not positive or has no currency, or a missing or repeated
// account, is ErrInvalidMovement.
func TestAChargeTheRulesCannotPost(t *testing.T) {
	customer, income := ids.New(), ids.New()
	cases := map[string]struct {
		account ids.ID
		charge  Charge
	}{
		"zero amount":      {customer, Charge{Amount: cop(t, 0), Counter: income}},
		"negative amount":  {customer, Charge{Amount: cop(t, -1), Counter: income}},
		"no currency":      {customer, Charge{Amount: money.Amount{}, Counter: income}},
		"no customer":      {ids.ID{}, Charge{Amount: cop(t, 1), Counter: income}},
		"no counter":       {customer, Charge{Amount: cop(t, 1)}},
		"the same account": {customer, Charge{Amount: cop(t, 1), Counter: customer}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			entries, err := Charges(tc.account, []Charge{{Amount: cop(t, 1), Counter: ids.New()}, tc.charge})
			if !errors.Is(err, ErrInvalidMovement) || entries != nil {
				t.Fatalf("Charges = %+v, %v; want ErrInvalidMovement and no entries", entries, err)
			}
		})
	}
}

// TestTheChargesBalance: whatever the charges, their entries add up to zero and the customer is debited their
// total.
func TestTheChargesBalance(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		customer := ids.New()
		units := rapid.SliceOfN(rapid.Int64Range(1, math.MaxInt64/16), 0, 8).Draw(rt, "units")
		charges := make([]Charge, len(units))
		for i, u := range units {
			charges[i] = Charge{Amount: cop(t, u), Counter: ids.New()}
		}
		entries, err := Charges(customer, charges)
		if err != nil {
			rt.Fatal(err)
		}
		var sum, debited int64
		for _, e := range entries {
			sum += e.Amount.Units()
			if e.AccountID == customer {
				debited += e.Amount.Units()
			}
		}
		var total int64
		for _, u := range units {
			total += u
		}
		if sum != 0 || debited != total || len(entries) != 2*len(units) {
			rt.Fatalf("entries %+v add up to %d and debit the customer %d, want 0 and %d", entries, sum, debited, total)
		}
	})
}

// TestATransferReceivedComesFromTheClearingAccount (FR-32): a credit and a suspense each debit the rail's
// clearing account and credit the customer's account or the rail's suspense account.
func TestATransferReceivedComesFromTheClearingAccount(t *testing.T) {
	clearing, customer, suspense := ids.New(), ids.New(), ids.New()
	cases := map[string]struct {
		rule     func(money.Amount, ids.ID, ids.ID) ([]ledger.EntryInput, error)
		credited ids.ID
	}{"a credit": {InboundCredit, customer}, "a suspense": {InboundSuspense, suspense}}
	for name, tc := range cases {
		entries, err := tc.rule(cop(t, 30000000), clearing, tc.credited)

		want := []ledger.EntryInput{{AccountID: clearing, Amount: cop(t, 30000000)}, {AccountID: tc.credited, Amount: cop(t, -30000000)}}
		if err != nil || !slices.Equal(entries, want) {
			t.Errorf("%s = %+v (%v), want %+v", name, entries, err, want)
		}
		if _, err := tc.rule(cop(t, 0), clearing, tc.credited); !errors.Is(err, ErrInvalidMovement) {
			t.Errorf("%s of nothing = %v, want ErrInvalidMovement", name, err)
		}
		if _, err := tc.rule(cop(t, 1), clearing, clearing); !errors.Is(err, ErrInvalidMovement) {
			t.Errorf("%s to the clearing account = %v, want ErrInvalidMovement", name, err)
		}
	}
}
