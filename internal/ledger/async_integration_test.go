//go:build integration

package ledger_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/postgres"
)

func aggregate(t *testing.T, batch int) int {
	t.Helper()
	n, err := svc().AggregateAsyncBalances(t.Context(), postgres.NewUnitOfWork(testPool), batch)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// unaggregated counts the async accounts whose balance is not the sum of their entries.
func unaggregated(t *testing.T) int {
	t.Helper()
	var n int
	err := testPool.QueryRow(t.Context(), `
		SELECT count(*) FROM ledger.accounts a JOIN ledger.balances b ON b.account_id = a.id
		WHERE a.balance_mode = 'async'
		  AND b.posted <> coalesce((SELECT sum(e.amount) FROM ledger.entries e WHERE e.account_id = a.id), 0)`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// catchUp aggregates until every async account adds up. The aggregator only takes what was posted by
// transactions older than the oldest one in progress on the server, and in CI the server is shared with
// other packages: it may need more than one run.
func catchUp(t *testing.T, batch int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for aggregate(t, batch); unaggregated(t) > 0; aggregate(t, batch) {
		if time.Now().After(deadline) {
			t.Fatalf("after 10 s, %d async accounts still do not add up", unaggregated(t))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestPostDoesNotCheckNorUpdateAsyncAccounts: a posting takes more out of an async account than it has
// without its limit being checked, and its materialized balance does not change until the aggregator
// runs, which does not touch the sync account's. TestPostSkipsAsyncAccounts, in app, covers that Post
// does not lock it.
func TestPostDoesNotCheckNorUpdateAsyncAccounts(t *testing.T) {
	cash, settlement := createAccount(t, ledger.KindAsset), asyncAccount(t, ledger.KindAsset)

	mustPostRef(t, transfer(t, cash.ID, settlement.ID, ids.New().String())) // the async asset ends at -100

	if b := balanceOf(t, settlement.ID); b.Posted != 0 || b.Version != 0 {
		t.Fatalf("async account balance before aggregating = %+v, want untouched", b)
	}
	catchUp(t, 100)
	if b := balanceOf(t, settlement.ID); b.Posted != -100 {
		t.Fatalf("async account balance after aggregating = %+v, want -100", b)
	}
	if b := balanceOf(t, cash.ID); b.Posted != 100 {
		t.Fatalf("cash (sync) after aggregating = %+v, want 100: the aggregator only touches async accounts", b)
	}
}

// TestAggregationMatchesTheEntries: after aggregating in small batches, each async account's balance is
// the sum of its entries, and a second run finds nothing and does not duplicate.
func TestAggregationMatchesTheEntries(t *testing.T) {
	clearing, income := asyncAccount(t, ledger.KindLiability), asyncAccount(t, ledger.KindIncome)
	for range 3 {
		deposit, _ := fundedDeposit(t)
		mustPostRef(t, transfer(t, deposit.ID, clearing.ID, ids.New().String()))
		mustPostRef(t, transfer(t, clearing.ID, income.ID, ids.New().String()))
	}

	catchUp(t, 2)
	before := balanceOf(t, clearing.ID)

	if n := aggregate(t, 2); n != 0 {
		t.Fatalf("the second run aggregated %d entries, want 0", n)
	}
	if after := balanceOf(t, clearing.ID); after != before || unaggregated(t) != 0 {
		t.Fatalf("balance after aggregating again = %+v, before %+v: it was duplicated", after, before)
	}
	if b := balanceOf(t, income.ID); b.Posted != -300 {
		t.Fatalf("income = %+v, want -300", b)
	}
}

// TestAPostingInFlightIsNotSkipped: a posting that took its entry id before another but commits later is
// not left behind the cursor: the aggregator does not go past the oldest transaction in progress.
func TestAPostingInFlightIsNotSkipped(t *testing.T) {
	clearing := asyncAccount(t, ledger.KindLiability)
	first, second := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindAsset)
	_, finishFirst := postAndHold(t, transfer(t, first.ID, clearing.ID, ids.New().String()))
	mustPostRef(t, transfer(t, second.ID, clearing.ID, ids.New().String()))

	aggregate(t, 100)
	if _, err := finishFirst(true); err != nil {
		t.Fatal(err)
	}

	catchUp(t, 100)
	if b := balanceOf(t, clearing.ID); b.Posted != -200 {
		t.Fatalf("clearing = %+v, want both postings (-200)", b)
	}
}

// TestAnOlderTransactionWithNewerEntriesIsAggregated: a database transaction that started earlier (lower
// xid) but posts its entries after another one has higher ids; the cursor follows xid order and does not
// leave it behind.
func TestAnOlderTransactionWithNewerEntriesIsAggregated(t *testing.T) {
	clearing := asyncAccount(t, ledger.KindLiability)
	older, newer := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindAsset)
	started, release, done := make(chan error, 1), make(chan struct{}), make(chan error, 1)
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock) // if the test fails earlier, the transaction is not left open stalling the aggregator
	go func() {
		done <- postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
			_, err := tx.Exec(t.Context(), `SELECT pg_current_xact_id()`) // takes its xid now
			started <- err
			<-release
			_, err = svc().Post(t.Context(), tx, transfer(t, older.ID, clearing.ID, ids.New().String()))
			return err
		})
	}()
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	mustPostRef(t, transfer(t, newer.ID, clearing.ID, ids.New().String()))
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	catchUp(t, 1)
	if b := balanceOf(t, clearing.ID); b.Posted != -200 {
		t.Fatalf("clearing = %+v, want both postings (-200)", b)
	}
}

// TestTheCursorOnlyMovesForward: not even with direct SQL can the application role move the cursor back
// or delete its row.
func TestTheCursorOnlyMovesForward(t *testing.T) {
	cases := map[string]struct{ sql, sqlState string }{
		"move back": {`UPDATE ledger.async_cursor SET posted_xact = '0', entry_id = 0`, "2F003"},
		"delete":    {`DELETE FROM ledger.async_cursor`, "42501"},
	}
	catchUp(t, 100) // the cursor already moved past the entries of this package's async accounts
	for name, tc := range cases {
		_, err := testPool.Exec(t.Context(), tc.sql)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != tc.sqlState {
			t.Errorf("%s = %v, want SQLSTATE %s", name, err, tc.sqlState)
		}
	}
}

func TestAggregateRejectsAnEmptyBatch(t *testing.T) {
	if _, err := svc().AggregateAsyncBalances(t.Context(), postgres.NewUnitOfWork(testPool), 0); err == nil {
		t.Fatal("a batch of 0 must fail: it would never advance")
	}
}
