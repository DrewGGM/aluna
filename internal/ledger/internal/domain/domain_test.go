package domain

import (
	"errors"
	"math"
	"slices"
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
		// Si el acumulador por moneda arrancara en Amount{} (sin moneda), Add lo rechazaría y este
		// caso fallaría: cada suma empieza en money.New(0, c).
		"dos entries balanceados":    {[]EntryInput{e(cash, 10_000_000, cop), e(deposit, -10_000_000, cop)}, nil},
		"desbalanceados":             {[]EntryInput{e(cash, 100, cop), e(deposit, -99, cop)}, ErrUnbalanced},
		"un entry":                   {[]EntryInput{e(cash, 100, cop)}, ErrTooFewEntries},
		"sin entries":                {nil, ErrTooFewEntries},
		"importe cero":               {[]EntryInput{e(cash, 0, cop), e(deposit, 0, cop)}, ErrZeroAmount},
		"dos monedas, cada una en 0": {[]EntryInput{e(cash, 5, cop), e(deposit, -5, cop), e(usdCash, 7, usd), e(usdDeposit, -7, usd)}, nil},
		"dos monedas cruzadas":       {[]EntryInput{e(cash, 5, cop), e(usdDeposit, -5, usd)}, ErrUnbalanced},
		"tres entries balanceados":   {[]EntryInput{e(cash, 100, cop), e(deposit, -60, cop), e(deposit, -40, cop)}, nil},
		"suma que se sale de int64":  {[]EntryInput{e(cash, math.MaxInt64, cop), e(cash, 1, cop), e(deposit, -1, cop)}, money.ErrOverflow},
		"entry sin moneda":           {[]EntryInput{{cash, money.New(5, money.Currency{})}, {deposit, money.New(-5, money.Currency{})}}, money.ErrCurrencyMismatch},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			txID := ids.New()

			tx, err := NewTransaction(NewTransactionInput{ID: txID, Entries: tc.entries})

			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, se esperaba %v", err, tc.want)
			}
			if err == nil && (tx.ID() != txID || !slices.Equal(tx.Entries(), tc.entries)) {
				t.Fatalf("transacción = %v %v", tx.ID(), tx.Entries())
			}
		})
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
		t.Fatalf("la transacción validada cambió desde afuera: %v", got)
	}
}

// TestEveryKindHasItsNature recorre las cinco clases: un retiro mayor que el saldo se rechaza en todas,
// y se rechaza por el lado de su naturaleza (débito para activo y gasto, crédito para el resto).
func TestEveryKindHasItsNature(t *testing.T) {
	cop := currency(t, "COP")
	outflow := map[Kind]int64{KindAsset: -101, KindExpense: -101, KindLiability: 101, KindEquity: 101, KindIncome: 101}
	for kind, delta := range outflow {
		a := Account{Code: string(kind), Kind: kind, Currency: cop}
		posted := NaturalBalance(kind, 100) // saldo natural 100 (la conversión es su propia inversa)

		if err := a.CheckAvailable(posted, 0, 0, delta); !errors.Is(err, ErrInsufficientFunds) {
			t.Errorf("%s: retirar 101 de 100 dio %v, se esperaba ErrInsufficientFunds", kind, err)
		}
		if err := a.CheckAvailable(posted, 0, 0, -delta); err != nil {
			t.Errorf("%s: un ingreso dio %v", kind, err)
		}
	}
}

func TestNaturalBalance(t *testing.T) {
	cases := map[Kind]int64{KindAsset: 500, KindExpense: 500, KindLiability: -500, KindEquity: -500, KindIncome: -500}
	for kind, want := range cases {
		if got := NaturalBalance(kind, 500); got != want {
			t.Errorf("NaturalBalance(%s, 500) = %d, se esperaba %d", kind, got, want)
		}
	}
}

func TestCheckAvailable(t *testing.T) {
	cop := currency(t, "COP")
	deposit := Account{Code: "ahorros-1", Kind: KindLiability, Currency: cop, Mode: BalanceModeSync}
	cash := Account{Code: "caja", Kind: KindAsset, Currency: cop, Mode: BalanceModeSync}
	overdraft := deposit
	overdraft.LowerLimit = -50_000
	// posted y delta con signo (débito +, crédito −), en unidades menores.
	cases := map[string]struct {
		account                               Account
		posted, pendingDebits, pendingCredits int64
		delta                                 int64
		want                                  error
	}{
		// Un depósito de 100_000 unidades deja posted = -100_000 en la cuenta del cliente (pasivo).
		"retiro dentro del saldo":                             {deposit, -100_000, 0, 0, 60_000, nil},
		"retiro de todo el saldo":                             {deposit, -100_000, 0, 0, 100_000, nil},
		"retiro mayor que el saldo":                           {deposit, -100_000, 0, 0, 100_001, ErrInsufficientFunds},
		"un hold saliente reduce el disponible":               {deposit, -100_000, 50_000, 0, 60_000, ErrInsufficientFunds},
		"un hold entrante no suma":                            {deposit, -100_000, 0, 500_000, 100_001, ErrInsufficientFunds},
		"sobregiro hasta el límite":                           {overdraft, -100_000, 0, 0, 150_000, nil},
		"sobregiro pasado el límite":                          {overdraft, -100_000, 0, 0, 150_001, ErrInsufficientFunds},
		"un depósito nunca se rechaza":                        {deposit, 30_000, 0, 0, -10_000, nil}, // cuenta ya bajo su límite
		"activo: salida mayor que el saldo":                   {cash, 100, 0, 0, -101, ErrInsufficientFunds},
		"activo: el hold saliente es un crédito":              {cash, 100, 0, 60, -50, ErrInsufficientFunds},
		"activo: entrada":                                     {cash, 0, 0, 0, 1_000, nil},
		"posted + delta se sale de int64":                     {cash, math.MinInt64, 0, 0, -1, money.ErrOverflow},
		"disponible menos holds se sale de int64":             {cash, math.MinInt64 + 10, 0, 100, -5, money.ErrOverflow},
		"pasivo: el crédito más grande no se lee como débito": {deposit, 0, 0, 0, math.MinInt64, nil},
		"pasivo bajo su límite, neto cero":                    {deposit, 30_000, 0, 0, 0, nil},
		"activo bajo su límite, neto cero":                    {cash, -1, 0, 0, 0, nil},
		"clase desconocida, ni siquiera un ingreso":           {Account{Code: "x", Currency: cop}, 0, 0, 0, 100, ErrUnknownKind},
		"pasivo: posted + delta se sale de int64":             {deposit, math.MaxInt64, 0, 0, 1, money.ErrOverflow},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := tc.account.CheckAvailable(tc.posted, tc.pendingDebits, tc.pendingCredits, tc.delta)

			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, se esperaba %v", err, tc.want)
			}
		})
	}
}
