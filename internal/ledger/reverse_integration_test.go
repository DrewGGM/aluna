//go:build integration

package ledger_test

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
)

const reason = "error de digitación"

// reversalDay es la fecha de negocio de las reversas: dos días después de la original, para que una
// reversa con la fecha valor equivocada se note.
var reversalDay = time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 2)

// tryReverse revierte in en su propia transacción. No llama t.Fatal: sirve desde otras goroutines.
func tryReverse(t *testing.T, in ledger.ReverseInput) (ledger.Transaction, error) {
	t.Helper()
	var reversal ledger.Transaction
	err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		var err error
		reversal, err = svc.Reverse(t.Context(), tx, in)
		return err
	})
	return reversal, err
}

func reverse(t *testing.T, original ids.ID) (ledger.Transaction, error) {
	t.Helper()
	return tryReverse(t, ledger.ReverseInput{
		LegalEntityID: entity, TransactionID: original, Reason: reason, Actor: "operador", BusinessDate: reversalDay,
	})
}

// TestReverseUndoesATransaction: la reversa invierte el signo de cada entry, apunta a la original y
// deja los saldos como estaban; leída de la base es la misma que devolvió Reverse.
func TestReverseUndoesATransaction(t *testing.T) {
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	original, _ := mustPostRef(t, transfer(t, cash.ID, deposit.ID, ids.New().String()))

	reversal, err := reverse(t, original.ID)
	if err != nil {
		t.Fatal(err)
	}

	c := cop(t)
	checkReversal(t, original, reversal,
		[]ledger.EntryInput{{AccountID: cash.ID, Amount: money.New(-100, c)}, {AccountID: deposit.ID, Amount: money.New(100, c)}})
	if pc, pd := postedOf(t, cash.ID), postedOf(t, deposit.ID); pc != 0 || pd != 0 {
		t.Fatalf("saldos tras la reversa = %d y %d, se esperaban 0 y 0", pc, pd)
	}
	for _, posted := range []ledger.Transaction{original, reversal} { // sin metadata y con metadata
		stored, err := svc.Transaction(t.Context(), testPool, entity, posted.ID)
		if err != nil || !reflect.DeepEqual(stored, posted) {
			t.Fatalf("leída de la base = %+v (%v), se esperaba la misma que se asentó %+v", stored, err, posted)
		}
	}
}

// checkReversal verifica que reversal apunte a original, guarde el motivo, tenga los entries want y
// lleve la fecha valor de la original y la de negocio de la reversa.
func checkReversal(t *testing.T, original, reversal ledger.Transaction, want []ledger.EntryInput) {
	t.Helper()
	if reversal.ReversesID != original.ID || reversal.Metadata["reason"] != reason || !slices.Equal(reversal.Entries, want) {
		t.Errorf("la reversa = %+v, debe apuntar a %s, guardar el motivo y tener los entries %+v", reversal, original.ID, want)
	}
	if !reversal.ValueDate.Equal(original.ValueDate) || !reversal.BusinessDate.Equal(reversalDay) {
		t.Errorf("fechas de la reversa = valor %v, negocio %v; se esperaban la fecha valor de la original %v y el negocio %v",
			reversal.ValueDate, reversal.BusinessDate, original.ValueDate, reversalDay)
	}
}

func TestATransactionIsReversedOnlyOnce(t *testing.T) {
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	original, _ := mustPostRef(t, transfer(t, cash.ID, deposit.ID, ids.New().String()))
	first, err := reverse(t, original.ID)
	if err != nil {
		t.Fatal(err)
	}

	again, err := reverse(t, original.ID)

	if !errors.Is(err, ledger.ErrAlreadyReversed) || again.ID != first.ID {
		t.Fatalf("segunda reversa = %s, %v; se esperaba ErrAlreadyReversed con la primera %s", again.ID, err, first.ID)
	}
	if pc := postedOf(t, cash.ID); pc != 0 {
		t.Fatalf("saldo de la caja = %d, se esperaba 0: la segunda reversa no mueve dinero", pc)
	}
}

// TestConcurrentReversalsOfAsyncAccountsReverseOnce: sin saldos que bloquear (cuentas async), las
// reversas concurrentes de una transacción chocan solo en la base; una pasa y las demás reciben
// ErrAlreadyReversed, nunca un 23505.
func TestConcurrentReversalsOfAsyncAccountsReverseOnce(t *testing.T) {
	a, b := accountOfMode(t, ledger.KindAsset, ledger.BalanceModeAsync), accountOfMode(t, ledger.KindLiability, ledger.BalanceModeAsync)
	original, _ := mustPostRef(t, transfer(t, a.ID, b.ID, ids.New().String()))

	errs := concurrently(t, 8, func(int) error { _, err := reverse(t, original.ID); return err })

	reversed := 0
	for _, err := range errs {
		switch {
		case err == nil:
			reversed++
		case !errors.Is(err, ledger.ErrAlreadyReversed):
			t.Errorf("reversa concurrente con un error inesperado: %v", err)
		}
	}
	if reversed != 1 {
		t.Fatalf("%d reversas pasaron, se esperaba 1", reversed)
	}
}

func accountOfMode(t *testing.T, kind ledger.Kind, mode ledger.BalanceMode) ledger.Account {
	t.Helper()
	a, err := tryCreateAccount(t, ledger.CreateAccountInput{
		LegalEntityID: entity, Code: "cta-" + ids.New().String(), Kind: kind, Currency: cop(t), Mode: mode,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestOnlyReversalsUseTheReversalSource: un posting cualquiera, también uno manual por la API, no puede
// ocupar el lugar de la reversa de otra transacción ni marcarse como reversa por su cuenta.
func TestOnlyReversalsUseTheReversalSource(t *testing.T) {
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	original, _ := mustPostRef(t, transfer(t, cash.ID, deposit.ID, ids.New().String()))
	squatter := transfer(t, cash.ID, deposit.ID, original.ID.String())
	squatter.Source = "ledger.reversal"
	selfReversal := transfer(t, deposit.ID, cash.ID, ids.New().String())
	selfReversal.ReversesID = original.ID
	otherRef := transfer(t, deposit.ID, cash.ID, ids.New().String())
	otherRef.Source, otherRef.ReversesID = "ledger.reversal", original.ID

	for name, in := range map[string]ledger.PostInput{
		"fuente de reversas": squatter, "ReversesID a mano": selfReversal, "reversa con otra referencia": otherRef,
	} {
		if _, _, err := postRef(t, in); !errors.Is(err, ledger.ErrInvalidPosting) {
			t.Errorf("%s = %v, se esperaba ErrInvalidPosting", name, err)
		}
	}
	if _, err := reverse(t, original.ID); err != nil {
		t.Fatalf("la original debe poder revertirse: %v", err)
	}
}

// TestReversingASpentDepositRespectsTheLimits: revertir un depósito cuyo dinero ya salió dejaría la
// cuenta en negativo: ErrInsufficientFunds, como cualquier posting, y los saldos no cambian.
func TestReversingASpentDepositRespectsTheLimits(t *testing.T) {
	cash, deposit, clearing := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability), createAccount(t, ledger.KindLiability)
	credited, _ := mustPostRef(t, transfer(t, cash.ID, deposit.ID, ids.New().String()))
	mustPostRef(t, transfer(t, deposit.ID, clearing.ID, ids.New().String()))

	if _, err := reverse(t, credited.ID); !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("revertir un depósito gastado = %v, se esperaba ErrInsufficientFunds", err)
	}
	if pc, pd := postedOf(t, cash.ID), postedOf(t, deposit.ID); pc != 100 || pd != 0 {
		t.Fatalf("saldos = %d y %d, se esperaban 100 y 0: la reversa rechazada no mueve dinero", pc, pd)
	}
}

// TestAReversalCanBeReversed: revertir una reversa vuelve a aplicar la original.
func TestAReversalCanBeReversed(t *testing.T) {
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	original, _ := mustPostRef(t, transfer(t, cash.ID, deposit.ID, ids.New().String()))
	reversal, err := reverse(t, original.ID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := reverse(t, reversal.ID); err != nil || postedOf(t, cash.ID) != 100 {
		t.Fatalf("revertir la reversa = %v, caja %d; se esperaba la caja en 100", err, postedOf(t, cash.ID))
	}
}

func TestReverseRejects(t *testing.T) {
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	original, _ := mustPostRef(t, transfer(t, cash.ID, deposit.ID, ids.New().String()))
	cases := map[string]struct {
		in   ledger.ReverseInput
		want error
	}{
		"transacción inexistente": {ledger.ReverseInput{LegalEntityID: entity, TransactionID: ids.New(), Reason: "r", Actor: "a", BusinessDate: time.Now()}, ledger.ErrTransactionNotFound},
		"de otra entidad":         {ledger.ReverseInput{LegalEntityID: ids.New(), TransactionID: original.ID, Reason: "r", Actor: "a", BusinessDate: time.Now()}, ledger.ErrTransactionNotFound},
		"sin motivo":              {ledger.ReverseInput{LegalEntityID: entity, TransactionID: original.ID, Actor: "a", BusinessDate: time.Now()}, ledger.ErrInvalidPosting},
		"sin actor":               {ledger.ReverseInput{LegalEntityID: entity, TransactionID: original.ID, Reason: "r", BusinessDate: time.Now()}, ledger.ErrInvalidPosting},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := tryReverse(t, tc.in); !errors.Is(err, tc.want) {
				t.Fatalf("= %v, se esperaba %v", err, tc.want)
			}
		})
	}
	if pc := postedOf(t, cash.ID); pc != 100 {
		t.Fatalf("saldo de la caja = %d, se esperaba 100: ninguna reversa rechazada mueve dinero", pc)
	}
}

// TestAReversalRefTakenBySQLFailsClosed: si alguien ocupa la referencia de la reversa por SQL directo
// (la base no lo impide), Reverse no da la transacción por revertida.
func TestAReversalRefTakenBySQLFailsClosed(t *testing.T) {
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	original, _ := mustPostRef(t, transfer(t, cash.ID, deposit.ID, ids.New().String()))
	err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		squatter := ids.New()
		if _, err := tx.Exec(t.Context(), `INSERT INTO ledger.transactions
			(id, legal_entity_id, code, value_date, business_date, source, external_ref, actor, channel, rule_version)
			VALUES ($1, $2, 'x', current_date, current_date, 'ledger.reversal', $3, 'sql', 'sql', 'v1')`,
			squatter, entity, original.ID.String()); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), `INSERT INTO ledger.entries (transaction_id, account_id, currency, amount)
			VALUES ($1, $2, 'COP', 1), ($1, $3, 'COP', -1)`, squatter, cash.ID, deposit.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := reverse(t, original.ID); err == nil || errors.Is(err, ledger.ErrAlreadyReversed) {
		t.Fatalf("= %v; con la referencia ocupada por otra transacción, Reverse debe fallar sin darla por revertida", err)
	}
}
