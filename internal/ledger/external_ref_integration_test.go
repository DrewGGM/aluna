//go:build integration

package ledger_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// transfer is a posting of 100 COP from from (credit) to to (debit) with reference ref.
func transfer(t *testing.T, to, from ids.ID, ref string) ledger.PostInput {
	t.Helper()
	today := time.Now().UTC().Truncate(24 * time.Hour)
	return ledger.PostInput{
		LegalEntityID: entity, Code: "test", ValueDate: today, BusinessDate: today,
		Source: "test", ExternalRef: ref, Actor: "tester", Channel: "test", RuleVersion: "v1",
		Entries: []ledger.EntryInput{
			{AccountID: to, Amount: money.New(100, cop(t))},
			{AccountID: from, Amount: money.New(-100, cop(t))},
		},
	}
}

// postRef posts in in its own transaction as an internal producer would: a duplicate is taken as done
// and committed. It does not call t.Fatal, so it works from other goroutines; err is Post's (if it is not
// a duplicate) or the commit's.
func postRef(t *testing.T, in ledger.PostInput) (posted ledger.Transaction, duplicate bool, err error) {
	t.Helper()
	err = postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		var postErr error
		posted, postErr = svc().Post(t.Context(), tx, in)
		if duplicate = errors.Is(postErr, ledger.ErrDuplicateExternalRef); duplicate {
			return nil
		}
		return postErr
	})
	return posted, duplicate, err
}

func mustPostRef(t *testing.T, in ledger.PostInput) (ledger.Transaction, bool) {
	t.Helper()
	posted, duplicate, err := postRef(t, in)
	if err != nil {
		t.Fatal(err)
	}
	return posted, duplicate
}

// countByRef counts the entity's transactions with reference ref and their entries.
func countByRef(t *testing.T, ref string) (transactions, entries int) {
	t.Helper()
	err := testPool.QueryRow(t.Context(), `SELECT count(DISTINCT t.id), count(e.id) FROM ledger.transactions t
		JOIN ledger.entries e ON e.transaction_id = t.id WHERE t.legal_entity_id = $1 AND t.external_ref = $2`,
		entity, ref).Scan(&transactions, &entries)
	if err != nil {
		t.Fatal(err)
	}
	return transactions, entries
}

// TestARepeatedExternalRefReturnsTheOriginal: the second Post with the same (entity, source,
// external_ref) returns the original transaction as a duplicate and writes nothing, even though it would
// no longer fit (the producer retries something that already went through).
func TestARepeatedExternalRefReturnsTheOriginal(t *testing.T) {
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	ref := ids.New().String()
	if _, duplicate := mustPostRef(t, transfer(t, cash.ID, deposit.ID, ids.New().String())); duplicate { // funds the cash account
		t.Fatal("the funding is not a duplicate")
	}
	original, duplicate := mustPostRef(t, transfer(t, deposit.ID, cash.ID, ref)) // empties the cash account
	if duplicate {
		t.Fatal("the first Post is not a duplicate")
	}

	again, duplicate := mustPostRef(t, transfer(t, deposit.ID, cash.ID, ref)) // there would be no funds anymore

	if !duplicate || again.ID != original.ID || !again.PostedAt.Equal(original.PostedAt) || len(again.Entries) != 2 {
		t.Fatalf("second Post = %+v (duplicate: %v), want the original %s", again, duplicate, original.ID)
	}
	if txs, entries := countByRef(t, ref); txs != 1 || entries != 2 {
		t.Fatalf("the reference has %d transactions and %d entries, want 1 and 2", txs, entries)
	}
	if got := postedOf(t, cash.ID); got != 0 {
		t.Fatalf("cash balance = %d, want 0: the duplicate moves no money", got)
	}
}

// TestConcurrentPostsWithTheSameRefPostOnce: 10 producers at once with the same reference post a single
// transaction; the others receive that same one as a duplicate, never an error.
func TestConcurrentPostsWithTheSameRefPostOnce(t *testing.T) {
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	ref := ids.New().String()
	posted, duplicates, errs := make([]ledger.Transaction, 10), make([]bool, 10), make([]error, 10)
	var wg sync.WaitGroup
	for i := range posted {
		wg.Go(func() { posted[i], duplicates[i], errs[i] = postRef(t, transfer(t, cash.ID, deposit.ID, ref)) })
	}
	wg.Wait()

	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	fresh := 0
	for i, p := range posted {
		if !duplicates[i] {
			fresh++
		}
		if p.ID != posted[0].ID {
			t.Errorf("producer %d received %s, want %s", i, p.ID, posted[0].ID)
		}
	}
	if txs, entries := countByRef(t, ref); fresh != 1 || txs != 1 || entries != 2 {
		t.Fatalf("%d producers posted; there are %d transactions and %d entries; want 1, 1 and 2", fresh, txs, entries)
	}
}

func TestTheSameRefFromAnotherSourceIsAnotherTransaction(t *testing.T) {
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	in := transfer(t, cash.ID, deposit.ID, ids.New().String())
	first, _ := mustPostRef(t, in)
	in.Source = "other-source"

	if second, duplicate := mustPostRef(t, in); duplicate || second.ID == first.ID {
		t.Fatalf("the same reference from another source = %s (duplicate: %v); want another transaction", second.ID, duplicate)
	}
}

// TestTheSameRefInAnotherEntityIsAnotherTransaction: uniqueness is per legal entity, also when both
// entities already have the reference.
func TestTheSameRefInAnotherEntityIsAnotherTransaction(t *testing.T) {
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	ref := ids.New().String()
	first, _ := mustPostRef(t, transfer(t, cash.ID, deposit.ID, ref))
	other := ids.New()
	otherCash := accountOf(t, other, ledger.KindAsset)
	otherDeposit := accountOf(t, other, ledger.KindLiability)
	in := transfer(t, otherCash.ID, otherDeposit.ID, ref)
	in.LegalEntityID = other

	second, duplicate := mustPostRef(t, in)
	if duplicate || second.ID == first.ID {
		t.Fatalf("the same reference in another entity = %s (duplicate: %v); want another transaction", second.ID, duplicate)
	}
	if again, duplicate := mustPostRef(t, in); !duplicate || again.ID != second.ID {
		t.Fatalf("repeating in the other entity = %s (duplicate: %v); want its original %s", again.ID, duplicate, second.ID)
	}
}

func accountOf(t *testing.T, legalEntity ids.ID, kind ledger.Kind) ledger.Account {
	t.Helper()
	a, err := tryCreateAccount(t, ledger.CreateAccountInput{
		LegalEntityID: legalEntity, Code: "acct-" + ids.New().String(), Kind: kind, Currency: cop(t), Mode: ledger.BalanceModeSync,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// postAndHold posts in in a transaction that stays open and returns its session's pid and a function that
// ends it (with commit or rollback) and returns what was posted.
func postAndHold(t *testing.T, in ledger.PostInput) (int32, func(commit bool) (ledger.Transaction, error)) {
	t.Helper()
	pids, done, release := make(chan int32, 1), make(chan error, 1), make(chan bool, 1)
	t.Cleanup(sync.OnceFunc(func() { release <- false })) // if the test fails earlier, the transaction is not left open
	var posted ledger.Transaction
	go func() {
		done <- postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
			var err error
			if posted, err = svc().Post(t.Context(), tx, in); err != nil {
				return err
			}
			var pid int32
			if err := tx.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return err
			}
			pids <- pid
			if !<-release {
				return errors.New("the first one rolls back")
			}
			return nil
		})
	}()
	select {
	case pid := <-pids:
		return pid, func(commit bool) (ledger.Transaction, error) {
			release <- commit
			err := <-done
			return posted, err
		}
	case err := <-done:
		t.Fatalf("the first producer did not post: %v", err)
		return 0, nil
	}
}

// raceAgainst posts second while first is still open, waits for second to be blocked by first, ends first
// (commit or rollback) and returns what second got.
func raceAgainst(t *testing.T, first, second ledger.PostInput, commit bool) (firstTx, secondTx ledger.Transaction, duplicate bool) {
	t.Helper()
	pid, finish := postAndHold(t, first)
	type result struct {
		posted    ledger.Transaction
		duplicate bool
		err       error
	}
	results := make(chan result, 1)
	go func() {
		var r result
		r.posted, r.duplicate, r.err = postRef(t, second)
		results <- r
	}()
	waitForBlockedBy(t, pid)

	firstTx, err := finish(commit)
	if (err == nil) != commit {
		t.Fatalf("the first transaction ended with %v", err)
	}
	r := <-results
	if r.err != nil {
		t.Fatalf("the second producer = %v", r.err)
	}
	return firstTx, r.posted, r.duplicate
}

// TestAProducerThatLosesTheRaceGetsTheOriginal: if another producer posts the same reference after the
// initial lookup, the second receives the original as a duplicate and writes nothing. On the same accounts
// it waits for the balance locks and, with the first one's commit, no longer has enough: the real case of
// a retry. On other accounts it waits for the reference's INSERT.
func TestAProducerThatLosesTheRaceGetsTheOriginal(t *testing.T) {
	for name, sameAccounts := range map[string]bool{"same accounts": true, "other accounts": false} {
		t.Run(name, func(t *testing.T) {
			cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
			if _, duplicate := mustPostRef(t, transfer(t, cash.ID, deposit.ID, ids.New().String())); duplicate { // funds 100
				t.Fatal("the funding is not a duplicate")
			}
			ref := ids.New().String()
			firstIn := transfer(t, deposit.ID, cash.ID, ref) // empties the cash account
			secondIn := firstIn                              // the retry: no longer has enough
			if !sameAccounts {
				otherCash, otherDeposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
				secondIn = transfer(t, otherCash.ID, otherDeposit.ID, ref) // funds: only waits for the INSERT
			}

			first, second, duplicate := raceAgainst(t, firstIn, secondIn, true)

			if !duplicate || second.ID != first.ID {
				t.Fatalf("the second = %s (duplicate: %v), want the original %s", second.ID, duplicate, first.ID)
			}
			if txs, entries := countByRef(t, ref); txs != 1 || entries != 2 {
				t.Fatalf("the reference has %d transactions and %d entries, want 1 and 2", txs, entries)
			}
		})
	}
}

// TestIfTheFirstRollsBackTheSecondPosts: if the first one holding the reference rolls back, the second,
// which was waiting for its INSERT, posts it.
func TestIfTheFirstRollsBackTheSecondPosts(t *testing.T) {
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	otherCash, otherDeposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	ref := ids.New().String()

	_, second, duplicate := raceAgainst(t, transfer(t, cash.ID, deposit.ID, ref), transfer(t, otherCash.ID, otherDeposit.ID, ref), false)

	if duplicate || second.ID == (ids.ID{}) || postedOf(t, otherCash.ID) != 100 {
		t.Fatalf("the second = %s (duplicate: %v); want it to post", second.ID, duplicate)
	}
	if txs, _ := countByRef(t, ref); txs != 1 {
		t.Fatalf("the reference has %d transactions, want 1", txs)
	}
}
