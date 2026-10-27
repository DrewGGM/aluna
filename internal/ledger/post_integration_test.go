//go:build integration

package ledger_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, &testPool)) }

var (
	svc    = ledger.New(calendar.New())
	entity = ids.New()
)

func cop(t *testing.T) money.Currency {
	t.Helper()
	c, err := money.CurrencyOf("COP")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func createAccount(t *testing.T, kind ledger.Kind) ledger.Account {
	t.Helper()
	account, err := tryCreateAccount(t, ledger.CreateAccountInput{
		LegalEntityID: entity, Code: "cta-" + ids.New().String(), Kind: kind, Currency: cop(t), Mode: ledger.BalanceModeSync,
	})
	if err != nil {
		t.Fatal(err)
	}
	return account
}

func tryCreateAccount(t *testing.T, in ledger.CreateAccountInput) (ledger.Account, error) {
	t.Helper()
	var account ledger.Account
	err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		var err error
		account, err = svc.CreateAccount(t.Context(), tx, in)
		return err
	})
	return account, err
}

// move asienta units desde from (crédito) hacia to (débito) y devuelve el resultado de Post.
func move(ctx context.Context, tx postgres.Tx, c money.Currency, to, from ids.ID, units int64, ref string) (ledger.Transaction, error) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	return svc.Post(ctx, tx, ledger.PostInput{
		LegalEntityID: entity, Code: "test", ValueDate: today, BusinessDate: today,
		Source: "test", ExternalRef: ref, Actor: "tester", Channel: "test", RuleVersion: "v1",
		Entries: []ledger.EntryInput{
			{AccountID: to, Amount: money.New(units, c)},
			{AccountID: from, Amount: money.New(-units, c)},
		},
	})
}

func postedOf(t *testing.T, account ids.ID) int64 {
	t.Helper()
	b, err := svc.Balance(t.Context(), testPool, account)
	if err != nil {
		t.Fatal(err)
	}
	return b.Posted
}

func TestPostUpdatesBalancesWithTheDatabaseClock(t *testing.T) {
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	before := time.Now()
	var posted ledger.Transaction

	err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		var err error
		posted, err = move(t.Context(), tx, cop(t), cash.ID, deposit.ID, 10_000_000, ids.New().String())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := postedOf(t, cash.ID); got != 10_000_000 {
		t.Errorf("caja posted = %d, se esperaba 10.000.000", got)
	}
	if got := postedOf(t, deposit.ID); got != -10_000_000 { // pasivo: saldo natural 10.000.000
		t.Errorf("depósito posted = %d, se esperaba -10.000.000", got)
	}
	var stored time.Time
	if err := testPool.QueryRow(t.Context(), `SELECT posted_at FROM ledger.transactions WHERE id = $1`, posted.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !stored.Equal(posted.PostedAt) || posted.PostedAt.Before(before.Add(-time.Minute)) {
		t.Errorf("posted_at devuelto %v, guardado %v: debe venir de la base", posted.PostedAt, stored)
	}
	b, err := svc.Balance(t.Context(), testPool, cash.ID)
	if err != nil || b.Version != 1 {
		t.Errorf("version = %d (%v), se esperaba 1 tras un posting", b.Version, err)
	}
}

func TestPostRejectsOverdraftAndPersistsNothing(t *testing.T) {
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	uow := postgres.NewUnitOfWork(testPool)
	if err := uow.Do(t.Context(), func(tx postgres.Tx) error {
		_, err := move(t.Context(), tx, cop(t), cash.ID, deposit.ID, 100_000, ids.New().String())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ref := ids.New().String()

	// Retiro de 100.001 unidades: débito al depósito del cliente, crédito a caja.
	err := uow.Do(t.Context(), func(tx postgres.Tx) error {
		_, err := move(t.Context(), tx, cop(t), deposit.ID, cash.ID, 100_001, ref)
		return err
	})

	if !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("error = %v, se esperaba ErrInsufficientFunds", err)
	}
	var n int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM ledger.transactions WHERE external_ref = $1`, ref).Scan(&n); err != nil || n != 0 {
		t.Fatalf("el retiro rechazado dejó %d transacciones (err = %v)", n, err)
	}
	if got := postedOf(t, deposit.ID); got != -100_000 {
		t.Fatalf("el saldo del depósito cambió a %d", got)
	}
}

func TestPostRejectsAccountsOfAnotherEntityOrCurrency(t *testing.T) {
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	foreign, err := tryCreateAccount(t, ledger.CreateAccountInput{
		LegalEntityID: ids.New(), Code: "ajena", Kind: ledger.KindAsset, Currency: cop(t), Mode: ledger.BalanceModeSync,
	})
	if err != nil {
		t.Fatal(err)
	}
	usd, err := money.CurrencyOf("USD")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		currency money.Currency
		to       ids.ID
		want     error
	}{
		"cuenta inexistente":          {cop(t), ids.New(), ledger.ErrAccountNotFound},
		"cuenta de otra entidad":      {cop(t), foreign.ID, ledger.ErrAccountNotFound},
		"moneda distinta a la cuenta": {usd, cash.ID, ledger.ErrCurrencyMismatch},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
				_, err := move(t.Context(), tx, tc.currency, tc.to, deposit.ID, 5, ids.New().String())
				return err
			})

			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, se esperaba %v", err, tc.want)
			}
		})
	}
}

// TestDeadlockIsRetried provoca un deadlock real: otra transacción toma los saldos en el orden inverso
// al de Post. PostgreSQL aborta a quien detecta el ciclo primero (Post, que empezó a esperar antes) y
// UnitOfWork.Do repite el posting completo.
func TestDeadlockIsRetried(t *testing.T) {
	first, second := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	other, err := testPool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Rollback(context.WithoutCancel(t.Context())) //nolint:errcheck // no hace nada tras el commit
	var otherPID int32
	if err := other.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&otherPID); err != nil {
		t.Fatal(err)
	}
	lock := `SELECT 1 FROM ledger.balances WHERE account_id = $1 FOR UPDATE`
	if _, err := other.Exec(t.Context(), lock, second.ID); err != nil { // Post bloquea first y luego second
		t.Fatal(err)
	}
	attempts := 0
	done := make(chan error, 1)
	go func() {
		done <- postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
			attempts++
			_, err := move(t.Context(), tx, cop(t), first.ID, second.ID, 100, ids.New().String())
			return err
		})
	}()
	waitForBlockedBy(t, otherPID) // Post ya tiene first y espera second, que tiene other

	if _, err := other.Exec(t.Context(), lock, first.ID); err != nil { // cierra el ciclo
		t.Fatalf("la otra transacción no debía ser la abortada: %v", err)
	}
	if err := other.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	if err := <-done; err != nil || attempts != 2 {
		t.Fatalf("Post terminó con %v tras %d intentos; se esperaba éxito en el segundo", err, attempts)
	}
}

// waitForBlockedBy espera hasta 10 s a que alguna sesión quede bloqueada por la sesión pid. El servidor
// puede ser compartido con otros paquetes en paralelo: contar cualquier espera no bastaría.
func waitForBlockedBy(t *testing.T, pid int32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var blocked int
		err := testPool.QueryRow(t.Context(),
			`SELECT count(*) FROM pg_locks WHERE NOT granted AND $1 = ANY(pg_blocking_pids(pid))`, pid).Scan(&blocked)
		if err != nil {
			t.Fatal(err)
		}
		if blocked > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Post nunca quedó esperando el lock de la otra transacción")
}

func TestCreateAccountRejectsADuplicateCode(t *testing.T) {
	first := createAccount(t, ledger.KindAsset)

	_, err := tryCreateAccount(t, ledger.CreateAccountInput{
		LegalEntityID: entity, Code: first.Code, Kind: ledger.KindAsset, Currency: cop(t), Mode: ledger.BalanceModeSync,
	})

	if !errors.Is(err, ledger.ErrInvalidAccount) {
		t.Fatalf("error = %v, se esperaba ErrInvalidAccount por el código repetido", err)
	}
}

func TestBalance(t *testing.T) {
	clearing, err := tryCreateAccount(t, ledger.CreateAccountInput{
		LegalEntityID: entity, Code: "clearing-" + ids.New().String(), Kind: ledger.KindAsset, Currency: cop(t),
		Mode: ledger.BalanceModeAsync,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Balance(t.Context(), testPool, clearing.ID); err != nil {
		t.Errorf("una cuenta async también tiene saldo materializado: %v", err)
	}
	if _, err := svc.Balance(t.Context(), testPool, ids.New()); !errors.Is(err, ledger.ErrAccountNotFound) {
		t.Errorf("error = %v, se esperaba ErrAccountNotFound para una cuenta que no existe", err)
	}
}
