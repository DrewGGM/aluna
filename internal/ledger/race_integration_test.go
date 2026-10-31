//go:build integration

package ledger_test

import (
	"errors"
	"maps"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// debit debits units to account and credits counterpart, in its own transaction and with a new
// reference. It does not call t.Fatal, so it works from other goroutines.
func debit(t *testing.T, c money.Currency, account, counterpart ids.ID, units int64) error {
	t.Helper()
	return postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		_, err := move(t.Context(), tx, c, account, counterpart, units, ids.New().String())
		return err
	})
}

// concurrently runs fn n times, releasing the goroutines together, and returns their errors. It first
// opens every pool connection: if the pool had to open them on the spot, the first transaction would
// finish alone and there would be no race to test. At most MaxConns of the pool's transactions compete at
// once; the rest wait for a connection.
func concurrently(t *testing.T, n int, fn func(i int) error) []error {
	t.Helper()
	warmUp(t)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() {
			<-start
			errs[i] = fn(i)
		})
	}
	close(start)
	wg.Wait()
	return errs
}

// warmUp opens every pool connection and returns them.
func warmUp(t *testing.T) {
	t.Helper()
	if testPool.Config().MaxConns < 2 {
		t.Fatal("with a single connection in the pool there is no race to test")
	}
	conns := make([]*pgxpool.Conn, 0, testPool.Config().MaxConns)
	defer func() {
		for _, conn := range conns {
			conn.Release()
		}
	}()
	for range cap(conns) {
		conn, err := testPool.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
	}
}

// netOfEntries sums, per currency, the entries of every transaction that touches account.
func netOfEntries(t *testing.T, account ids.ID) map[string]int64 {
	t.Helper()
	rows, err := testPool.Query(t.Context(), `SELECT currency, sum(amount)::bigint FROM ledger.entries
		WHERE transaction_id IN (SELECT transaction_id FROM ledger.entries WHERE account_id = $1)
		GROUP BY currency`, account)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	net := map[string]int64{}
	for rows.Next() {
		var (
			currency string
			sum      int64
		)
		if err := rows.Scan(&currency, &sum); err != nil {
			t.Fatal(err)
		}
		net[currency] = sum
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return net
}

// outcomes counts the withdrawals that went through and those rejected for funds; any other error fails.
func outcomes(t *testing.T, errs []error) (succeeded, insufficient int) {
	t.Helper()
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ledger.ErrInsufficientFunds):
			insufficient++
		default:
			t.Errorf("withdrawal with an unexpected error: %v", err)
		}
	}
	return succeeded, insufficient
}

// TestRaceDebits (QS-1): 50 withdrawals of 70,000 compete on a deposit of 100,000: exactly one goes
// through and the rest are insufficient funds. Without the balances' FOR UPDATE, the competitors would
// read 100,000 at once and go through: the deposit would be overdrawn. The counterpart is an async account
// (neither locked nor updated in the posting): with two sync accounts, without the FOR UPDATE their
// UPDATEs would block in a cycle, the 40P01 would be retried with the balance already committed and would
// hide the overdraft.
func TestRaceDebits(t *testing.T) {
	c := cop(t)
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	clearing, err := tryCreateAccount(t, ledger.CreateAccountInput{
		LegalEntityID: entity, Code: "bridge-" + ids.New().String(), Kind: ledger.KindLiability, Currency: c,
		Mode: ledger.BalanceModeAsync,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := debit(t, c, cash.ID, deposit.ID, 10_000_000); err != nil { // deposit with natural balance 100,000
		t.Fatal(err)
	}

	errs := concurrently(t, 50, func(int) error { return debit(t, c, deposit.ID, clearing.ID, 7_000_000) }) // withdrawals of 70,000

	if succeeded, insufficient := outcomes(t, errs); succeeded != 1 || insufficient != 49 {
		t.Fatalf("%d withdrawals went through and %d were insufficient funds; want 1 and 49", succeeded, insufficient)
	}
	if natural := ledger.NaturalBalance(ledger.KindLiability, postedOf(t, deposit.ID)); natural != 3_000_000 {
		t.Fatalf("deposit natural balance = %d, want 3,000,000 (30,000 COP)", natural)
	}
	if net := netOfEntries(t, deposit.ID); !maps.Equal(net, map[string]int64{"COP": 0}) {
		t.Fatalf("Σ entries per currency = %v, want COP: 0", net)
	}
	var entries int64
	if err := testPool.QueryRow(t.Context(), `SELECT sum(amount)::bigint FROM ledger.entries WHERE account_id = $1`, deposit.ID).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if posted := postedOf(t, deposit.ID); entries != posted {
		t.Fatalf("Σ deposit entries = %d and its materialized balance = %d: they must match", entries, posted)
	}
}

// TestRaceCrossTransfers: A→B and B→A transfers at once all finish, with no deadlock or wait that
// exhausts the lock_timeout, because balances are locked in account order (ADR-0004). TestDeadlockIsRetried
// covers the UnitOfWork retry on a 40P01.
func TestRaceCrossTransfers(t *testing.T) {
	c := cop(t)
	a, b, funding := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	for _, account := range []ids.ID{a.ID, b.ID} {
		if err := debit(t, c, account, funding.ID, 1_000_000); err != nil {
			t.Fatal(err)
		}
	}

	errs := concurrently(t, 40, func(i int) error {
		if i%2 == 1 {
			return debit(t, c, a.ID, b.ID, 1_000)
		}
		return debit(t, c, b.ID, a.ID, 1_000)
	})

	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if pa, pb := postedOf(t, a.ID), postedOf(t, b.ID); pa != 1_000_000 || pb != 1_000_000 {
		t.Fatalf("balances = %d and %d, want 1,000,000 and 1,000,000", pa, pb)
	}
}
