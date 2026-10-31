package domain

import (
	"errors"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/DrewGGM/aluna/internal/ids"
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

func TestNewTransaction(t *testing.T) {
	cop, usd := currency(t, "COP"), currency(t, "USD")
	cash, deposit, usdCash, usdDeposit := ids.New(), ids.New(), ids.New(), ids.New()
	e := func(account ids.ID, units int64, c money.Currency) EntryInput {
		return EntryInput{AccountID: account, Amount: money.New(units, c)}
	}
	cases := map[string]struct {
		entries []EntryInput
		want    error
	}{
		// If the per-currency accumulator started at Amount{} (no currency), Add would reject it and this
		// case would fail: each sum starts at money.New(0, c).
		"two balanced entries":      {[]EntryInput{e(cash, 10_000_000, cop), e(deposit, -10_000_000, cop)}, nil},
		"unbalanced":                {[]EntryInput{e(cash, 100, cop), e(deposit, -99, cop)}, ErrUnbalanced},
		"one entry":                 {[]EntryInput{e(cash, 100, cop)}, ErrTooFewEntries},
		"no entries":                {nil, ErrTooFewEntries},
		"zero amount":               {[]EntryInput{e(cash, 0, cop), e(deposit, 0, cop)}, ErrZeroAmount},
		"two currencies, each at 0": {[]EntryInput{e(cash, 5, cop), e(deposit, -5, cop), e(usdCash, 7, usd), e(usdDeposit, -7, usd)}, nil},
		"two crossed currencies":    {[]EntryInput{e(cash, 5, cop), e(usdDeposit, -5, usd)}, ErrUnbalanced},
		"three balanced entries":    {[]EntryInput{e(cash, 100, cop), e(deposit, -60, cop), e(deposit, -40, cop)}, nil},
		"sum that overflows int64":  {[]EntryInput{e(cash, math.MaxInt64, cop), e(cash, 1, cop), e(deposit, -1, cop)}, money.ErrOverflow},
		"entry without a currency":  {[]EntryInput{{cash, money.New(5, money.Currency{})}, {deposit, money.New(-5, money.Currency{})}}, money.ErrCurrencyMismatch},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			txID := ids.New()

			tx, err := NewTransaction(NewTransactionInput{ID: txID, Entries: tc.entries})

			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if err == nil && (tx.ID() != txID || !slices.Equal(tx.Entries(), tc.entries)) {
				t.Fatalf("transaction = %v %v", tx.ID(), tx.Entries())
			}
		})
	}
}

// TestUnbalancedErrorNamesTheFirstCurrency pins the message: with several unbalanced currencies it names
// the first one in entry order, always the same one (reproducible logs and responses).
func TestUnbalancedErrorNamesTheFirstCurrency(t *testing.T) {
	cop, usd := currency(t, "COP"), currency(t, "USD")
	entries := []EntryInput{
		{ids.New(), money.New(1, cop)},
		{ids.New(), money.New(1, usd)},
		{ids.New(), money.New(2, cop)},
		{ids.New(), money.New(2, usd)},
	}
	for range 50 {
		_, err := NewTransaction(NewTransactionInput{ID: ids.New(), Entries: entries})
		if !errors.Is(err, ErrUnbalanced) || !strings.Contains(err.Error(), "COP sums to 3") {
			t.Fatalf("error = %v, want it to name COP", err)
		}
	}
}

func TestTransactionEntriesCannotBeChangedFromOutside(t *testing.T) {
	cop := currency(t, "COP")
	in := []EntryInput{{ids.New(), money.New(1, cop)}, {ids.New(), money.New(-1, cop)}}
	tx, err := NewTransaction(NewTransactionInput{ID: ids.New(), Entries: in})
	if err != nil {
		t.Fatal(err)
	}

	in[0].Amount = money.New(1_000_000, cop)
	tx.Entries()[1].Amount = money.New(-1_000_000, cop)

	if got := tx.Entries(); got[0].Amount.Units() != 1 || got[1].Amount.Units() != -1 {
		t.Fatalf("the validated transaction changed from outside: %v", got)
	}
}

// TestEveryKindHasItsNature goes through the five kinds: a withdrawal larger than the balance is rejected
// in all of them, and it is rejected on their normal side (debit for asset and expense, credit for the
// rest).
func TestEveryKindHasItsNature(t *testing.T) {
	cop := currency(t, "COP")
	outflow := map[Kind]int64{KindAsset: -101, KindExpense: -101, KindLiability: 101, KindEquity: 101, KindIncome: 101}
	for kind, delta := range outflow {
		a := Account{Code: string(kind), Kind: kind, Currency: cop}
		posted := NaturalBalance(kind, 100) // natural balance 100 (the conversion is its own inverse)

		if err := a.CheckAvailable(posted, 0, 0, delta); !errors.Is(err, ErrInsufficientFunds) {
			t.Errorf("%s: withdrawing 101 from 100 gave %v, want ErrInsufficientFunds", kind, err)
		}
		if err := a.CheckAvailable(posted, 0, 0, -delta); err != nil {
			t.Errorf("%s: an inflow gave %v", kind, err)
		}
	}
}

func TestNaturalBalance(t *testing.T) {
	cases := map[Kind]int64{KindAsset: 500, KindExpense: 500, KindLiability: -500, KindEquity: -500, KindIncome: -500}
	for kind, want := range cases {
		if got := NaturalBalance(kind, 500); got != want {
			t.Errorf("NaturalBalance(%s, 500) = %d, want %d", kind, got, want)
		}
	}
}

func TestCheckAvailable(t *testing.T) {
	cop := currency(t, "COP")
	deposit := Account{Code: "savings-1", Kind: KindLiability, Currency: cop, Mode: BalanceModeSync}
	cash := Account{Code: "cash", Kind: KindAsset, Currency: cop, Mode: BalanceModeSync}
	overdraft := deposit
	overdraft.LowerLimit = -50_000
	// posted and delta are signed (debit +, credit −), in minor units.
	cases := map[string]struct {
		account                               Account
		posted, pendingDebits, pendingCredits int64
		delta                                 int64
		want                                  error
	}{
		// A 100_000-unit deposit leaves posted = -100_000 on the customer account (liability).
		"withdrawal within the balance":                      {deposit, -100_000, 0, 0, 60_000, nil},
		"withdrawal of the whole balance":                    {deposit, -100_000, 0, 0, 100_000, nil},
		"withdrawal larger than the balance":                 {deposit, -100_000, 0, 0, 100_001, ErrInsufficientFunds},
		"an outgoing hold reduces the available balance":     {deposit, -100_000, 50_000, 0, 60_000, ErrInsufficientFunds},
		"an incoming hold does not add":                      {deposit, -100_000, 0, 500_000, 100_001, ErrInsufficientFunds},
		"overdraft up to the limit":                          {overdraft, -100_000, 0, 0, 150_000, nil},
		"overdraft past the limit":                           {overdraft, -100_000, 0, 0, 150_001, ErrInsufficientFunds},
		"a deposit is never rejected":                        {deposit, 30_000, 0, 0, -10_000, nil}, // account already below its limit
		"asset: outflow larger than the balance":             {cash, 100, 0, 0, -101, ErrInsufficientFunds},
		"asset: the outgoing hold is a credit":               {cash, 100, 0, 60, -50, ErrInsufficientFunds},
		"asset: inflow":                                      {cash, 0, 0, 0, 1_000, nil},
		"posted + delta overflows int64":                     {cash, math.MinInt64, 0, 0, -1, money.ErrOverflow},
		"available minus holds overflows int64":              {cash, math.MinInt64 + 10, 0, 100, -5, money.ErrOverflow},
		"liability: the largest credit is not read as debit": {deposit, 0, 0, 0, math.MinInt64, nil},
		"liability below its limit, zero net":                {deposit, 30_000, 0, 0, 0, nil},
		"asset below its limit, zero net":                    {cash, -1, 0, 0, 0, nil},
		"unknown kind, not even an inflow":                   {Account{Code: "x", Currency: cop}, 0, 0, 0, 100, ErrUnknownKind},
		"liability: posted + delta overflows int64":          {deposit, math.MaxInt64, 0, 0, 1, money.ErrOverflow},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := tc.account.CheckAvailable(tc.posted, tc.pendingDebits, tc.pendingCredits, tc.delta)

			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}
