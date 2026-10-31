//go:build integration

package ledger_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, &testPool)) }

var entity = ids.New()

// svc is the tests' ledger, with its events going to the outbox; it is created the first time, once the
// database exists.
var svc = sync.OnceValue(func() *ledger.Service {
	events, err := outbox.New(testPool)
	if err != nil {
		panic(err)
	}
	return ledger.New(calendar.New(), events)
})

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
		LegalEntityID: entity, Code: "acct-" + ids.New().String(), Kind: kind, Currency: cop(t), Mode: ledger.BalanceModeSync,
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
		account, err = svc().CreateAccount(t.Context(), tx, in)
		return err
	})
	return account, err
}

// move posts units from from (credit) to to (debit) and returns Post's result.
func move(ctx context.Context, tx postgres.Tx, c money.Currency, to, from ids.ID, units int64, ref string) (ledger.Transaction, error) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	return svc().Post(ctx, tx, ledger.PostInput{
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
	b, err := svc().Balance(t.Context(), testPool, account)
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
		t.Errorf("cash posted = %d, want 10,000,000", got)
	}
	if got := postedOf(t, deposit.ID); got != -10_000_000 { // liability: natural balance 10,000,000
		t.Errorf("deposit posted = %d, want -10,000,000", got)
	}
	var stored time.Time
	if err := testPool.QueryRow(t.Context(), `SELECT posted_at FROM ledger.transactions WHERE id = $1`, posted.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !stored.Equal(posted.PostedAt) || posted.PostedAt.Before(before.Add(-time.Minute)) {
		t.Errorf("posted_at returned %v, stored %v: it must come from the database", posted.PostedAt, stored)
	}
	b, err := svc().Balance(t.Context(), testPool, cash.ID)
	if err != nil || b.Version != 1 {
		t.Errorf("version = %d (%v), want 1 after a posting", b.Version, err)
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

	// Withdrawal of 100,001 units: debit to the customer's deposit, credit to cash.
	err := uow.Do(t.Context(), func(tx postgres.Tx) error {
		_, err := move(t.Context(), tx, cop(t), deposit.ID, cash.ID, 100_001, ref)
		return err
	})

	if !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("error = %v, want ErrInsufficientFunds", err)
	}
	var n int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM ledger.transactions WHERE external_ref = $1`, ref).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the rejected withdrawal left %d transactions (err = %v)", n, err)
	}
	if got := postedOf(t, deposit.ID); got != -100_000 {
		t.Fatalf("the deposit balance changed to %d", got)
	}
}

func TestPostRejectsAccountsOfAnotherEntityOrCurrency(t *testing.T) {
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	foreign, err := tryCreateAccount(t, ledger.CreateAccountInput{
		LegalEntityID: ids.New(), Code: "foreign", Kind: ledger.KindAsset, Currency: cop(t), Mode: ledger.BalanceModeSync,
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
		"nonexistent account":               {cop(t), ids.New(), ledger.ErrAccountNotFound},
		"another entity's account":          {cop(t), foreign.ID, ledger.ErrAccountNotFound},
		"currency other than the account's": {usd, cash.ID, ledger.ErrCurrencyMismatch},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
				_, err := move(t.Context(), tx, tc.currency, tc.to, deposit.ID, 5, ids.New().String())
				return err
			})

			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestDeadlockIsRetried causes a real deadlock: another transaction takes the balances in the reverse
// order of Post. PostgreSQL aborts whoever detects the cycle first (Post, which started waiting earlier)
// and UnitOfWork.Do repeats the whole posting.
func TestDeadlockIsRetried(t *testing.T) {
	first, second := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	other, err := testPool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Rollback(context.WithoutCancel(t.Context())) //nolint:errcheck // a no-op after the commit
	var otherPID int32
	if err := other.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&otherPID); err != nil {
		t.Fatal(err)
	}
	lock := `SELECT 1 FROM ledger.balances WHERE account_id = $1 FOR UPDATE`
	if _, err := other.Exec(t.Context(), lock, second.ID); err != nil { // Post locks first and then second
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
	waitForBlockedBy(t, otherPID) // Post already holds first and waits for second, which other holds

	if _, err := other.Exec(t.Context(), lock, first.ID); err != nil { // closes the cycle
		t.Fatalf("the other transaction should not have been the aborted one: %v", err)
	}
	if err := other.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	if err := <-done; err != nil || attempts != 2 {
		t.Fatalf("Post ended with %v after %d attempts; want success on the second", err, attempts)
	}
}

// waitForBlockedBy waits up to 10 s for some session to be blocked by session pid. The server may be
// shared with other packages in parallel: counting any wait would not be enough.
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
	t.Fatal("Post never ended up waiting for the other transaction's lock")
}

func TestCreateAccountRejectsADuplicateCode(t *testing.T) {
	first := createAccount(t, ledger.KindAsset)

	_, err := tryCreateAccount(t, ledger.CreateAccountInput{
		LegalEntityID: entity, Code: first.Code, Kind: ledger.KindAsset, Currency: cop(t), Mode: ledger.BalanceModeSync,
	})

	if !errors.Is(err, ledger.ErrInvalidAccount) {
		t.Fatalf("error = %v, want ErrInvalidAccount for the repeated code", err)
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

	if _, err := svc().Balance(t.Context(), testPool, clearing.ID); err != nil {
		t.Errorf("an async account also has a materialized balance: %v", err)
	}
	if _, err := svc().Balance(t.Context(), testPool, ids.New()); !errors.Is(err, ledger.ErrAccountNotFound) {
		t.Errorf("error = %v, want ErrAccountNotFound for an account that does not exist", err)
	}
}
