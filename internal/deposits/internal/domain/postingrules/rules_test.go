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

func currency(t *testing.T, code string) money.Currency {
	t.Helper()
	c, err := money.CurrencyOf(code)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// ruleCase is a deposit rule with the account it debits (+) and the one it credits (−).
type ruleCase struct {
	name          string
	apply         func(Movement) ([]ledger.EntryInput, error)
	debit, credit ids.ID
}

func rules(customer, counter ids.ID) []ruleCase {
	return []ruleCase{
		{"cash deposit", CashDeposit, counter, customer},
		{"withdrawal", Withdrawal, customer, counter},
		{"internal transfer", InternalTransfer, customer, counter},
		{"interest capitalization", InterestCapitalization, counter, customer},
		{"fee charge", FeeCharge, customer, counter},
		{"GMF withholding", GMFWithholding, customer, counter},
	}
}

// TestEachRulePostsItsEntries: each rule debits and credits the expected accounts with the movement's amount.
func TestEachRulePostsItsEntries(t *testing.T) {
	customer, counter := ids.New(), ids.New()
	cop := currency(t, "COP")
	for _, r := range rules(customer, counter) {
		t.Run(r.name, func(t *testing.T) {
			entries, err := r.apply(Movement{Amount: money.New(1000000, cop), Account: customer, Counter: counter})
			if err != nil {
				t.Fatal(err)
			}
			want := []ledger.EntryInput{
				{AccountID: r.debit, Amount: money.New(1000000, cop)},
				{AccountID: r.credit, Amount: money.New(-1000000, cop)},
			}
			if !slices.Equal(entries, want) {
				t.Fatalf("entries = %+v, want %+v", entries, want)
			}
		})
	}
}

// TestAMovementMustBeValid: an amount that is not positive or has no currency, a missing account or one
// account on both sides is ErrInvalidMovement for every rule.
func TestAMovementMustBeValid(t *testing.T) {
	customer, counter := ids.New(), ids.New()
	cop := currency(t, "COP")
	invalid := map[string]Movement{
		"zero amount":      {Amount: money.New(0, cop), Account: customer, Counter: counter},
		"negative amount":  {Amount: money.New(-1, cop), Account: customer, Counter: counter},
		"no currency":      {Amount: money.New(1, money.Currency{}), Account: customer, Counter: counter},
		"no account":       {Amount: money.New(1, cop), Counter: counter},
		"no counter":       {Amount: money.New(1, cop), Account: customer},
		"the same account": {Amount: money.New(1, cop), Account: customer, Counter: customer},
	}
	for _, r := range rules(customer, counter) {
		for what, m := range invalid {
			if _, err := r.apply(m); !errors.Is(err, ErrInvalidMovement) {
				t.Errorf("%s with %s = %v, want ErrInvalidMovement", r.name, what, err)
			}
		}
	}
}

// TestEveryRuleBalances (property): for any positive amount, up to the largest int64, in any currency, the
// entries of every rule are at least two, in the movement's currency, and sum to zero.
func TestEveryRuleBalances(t *testing.T) {
	currencies := []money.Currency{currency(t, "COP"), currency(t, "USD"), currency(t, "JPY"), currency(t, "CLF")}
	customer, counter := ids.New(), ids.New()
	rapid.Check(t, func(rt *rapid.T) {
		c := rapid.SampledFrom(currencies).Draw(rt, "currency")
		amount := money.New(rapid.Int64Range(1, math.MaxInt64).Draw(rt, "units"), c)
		for _, r := range rules(customer, counter) {
			entries, err := r.apply(Movement{Amount: amount, Account: customer, Counter: counter})
			if err != nil {
				rt.Fatalf("%s: %v", r.name, err)
			}
			var total int64
			for _, e := range entries {
				if e.Amount.Currency() != c {
					rt.Fatalf("%s: entry in %s, want %s", r.name, e.Amount.Currency().Code(), c.Code())
				}
				total += e.Amount.Units() // +x and −x: it cannot overflow
			}
			if total != 0 || len(entries) < 2 {
				rt.Fatalf("%s: %d entries summing %d, want at least two summing 0", r.name, len(entries), total)
			}
		}
	})
}
