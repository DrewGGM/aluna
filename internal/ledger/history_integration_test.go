//go:build integration

package ledger_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// historyOf reads the history of account as one of legalEntity's limit entries at a time, from the newest,
// and returns every entry and the size of each page.
func historyOf(t *testing.T, legalEntity, account ids.ID, limit int) (entries []ledger.HistoryEntry, pages []int) {
	t.Helper()
	cursor := ""
	for {
		page, err := svc().History(t.Context(), testPool, legalEntity, account, cursor, limit)
		if err != nil {
			t.Fatal(err)
		}
		entries, pages = append(entries, page.Entries...), append(pages, len(page.Entries))
		if page.Next == "" {
			return entries, pages
		}
		cursor = page.Next
	}
}

// thousandMovements posts 1,000 credits of 100 COP to deposit from cash, in one database transaction, and
// returns their IDs.
func thousandMovements(t *testing.T, cash, deposit ids.ID) map[ids.ID]bool {
	t.Helper()
	posted := make(map[ids.ID]bool)
	err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		for range 1000 {
			p, err := svc().Post(t.Context(), tx, transfer(t, cash, deposit, ids.New().String()))
			if err != nil {
				return err
			}
			posted[p.ID] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return posted
}

// postingMeanwhile posts credits of 100 COP to deposit from cash, one after another, from when it returns
// until stop is called; stop returns the error of the posting that failed, if any.
func postingMeanwhile(t *testing.T, cash, deposit ids.ID) (stop func() error) {
	t.Helper()
	in := transfer(t, cash, deposit, "")
	quit, started, done := make(chan struct{}), make(chan struct{}), make(chan error, 1) // the goroutine ends even if the test does first
	go func() {
		for first := true; ; first = false {
			select {
			case <-quit:
				done <- nil
				return
			default:
			}
			in.ExternalRef = ids.New().String()
			if _, _, err := postRef(t, in); err != nil {
				done <- err
				return
			}
			if first {
				close(started)
			}
		}
	}()
	select {
	case <-started:
	case err := <-done:
		t.Fatal(err)
	}
	return func() error {
		close(quit)
		return <-done
	}
}

// TestTheHistoryPagesThroughEveryEntryOnce (FR-44): an account's 1,000 movements, read 50 at a time while
// others are posted on it from before the first page, come back each once and newest first.
func TestTheHistoryPagesThroughEveryEntryOnce(t *testing.T) {
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	posted := thousandMovements(t, cash.ID, deposit.ID)
	stop := postingMeanwhile(t, cash.ID, deposit.ID)

	entries, pages := historyOf(t, entity, deposit.ID, 50)

	if err := stop(); err != nil {
		t.Fatal(err)
	}
	expectEachOnce(t, entries, posted)
	if i := slices.IndexFunc(pages[:len(pages)-1], func(n int) bool { return n != 50 }); i >= 0 {
		t.Fatalf("page %d has %d entries, want 50", i, pages[i])
	}
}

// expectEachOnce checks that entries, newest first, are credits of 100 COP of different transactions, and
// that every one of posted is among them.
func expectEachOnce(t *testing.T, entries []ledger.HistoryEntry, posted map[ids.ID]bool) {
	t.Helper()
	seen := make(map[ids.ID]bool)
	for i, e := range entries {
		if seen[e.TransactionID] || e.Amount != money.New(-100, cop(t)) || e.Code != "test" {
			t.Fatalf("entry %d = %+v, want a credit of 100 COP of code test seen once", i, e)
		}
		seen[e.TransactionID] = true
		if i > 0 && e.PostedAt.After(entries[i-1].PostedAt) {
			t.Fatalf("entry %d (%s) is newer than the one before it (%s)", i, e.PostedAt, entries[i-1].PostedAt)
		}
	}
	for id := range posted {
		if !seen[id] {
			t.Fatalf("transaction %s is missing from the history", id)
		}
	}
}

// TestTheEntriesOfATransactionFollowTheirOrder (FR-44): the entries of one transaction share its posted_at
// and come back newest first by the order they were posted, one per page with a limit of 1; the last page
// has no next cursor, and neither does that of an account without entries or of another entity.
func TestTheEntriesOfATransactionFollowTheirOrder(t *testing.T) {
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	in := transfer(t, cash.ID, deposit.ID, ids.New().String())
	in.Entries = []ledger.EntryInput{
		{AccountID: cash.ID, Amount: money.New(300, cop(t))},
		{AccountID: deposit.ID, Amount: money.New(-100, cop(t))},
		{AccountID: deposit.ID, Amount: money.New(-200, cop(t))},
	}
	p, _ := mustPostRef(t, in)

	entries, pages := historyOf(t, entity, deposit.ID, 1)
	_, empty := historyOf(t, entity, createAccount(t, ledger.KindLiability).ID, 1)
	_, elsewhere := historyOf(t, ids.New(), deposit.ID, 1)

	line := func(units int64) string {
		return fmt.Sprintf("%s %s %d on %s at %s", p.ID, "test", units, in.ValueDate.Format(time.DateOnly), p.PostedAt.UTC())
	}
	got := make([]string, len(entries))
	for i, e := range entries {
		got[i] = fmt.Sprintf("%s %s %d on %s at %s", e.TransactionID, e.Code, e.Amount.Units(), e.ValueDate.Format(time.DateOnly),
			e.PostedAt.UTC())
	}
	if want := []string{line(-200), line(-100)}; !slices.Equal(got, want) {
		t.Fatalf("history = %q, want %q", got, want)
	}
	if !slices.Equal(pages, []int{1, 1}) || !slices.Equal(empty, []int{0}) || !slices.Equal(elsewhere, []int{0}) {
		t.Fatalf("pages = %v, %v without entries and %v of another entity, want [1 1], [0] and [0]", pages, empty, elsewhere)
	}
}

// TestAPageIsOfUpTo200Entries (FR-44): a page has from 1 to 200 entries; anything else is ErrInvalidPage.
func TestAPageIsOfUpTo200Entries(t *testing.T) {
	account := createAccount(t, ledger.KindLiability).ID
	for limit, want := range map[int]error{200: nil, 201: ledger.ErrInvalidPage, 0: ledger.ErrInvalidPage} {
		if _, err := svc().History(t.Context(), testPool, entity, account, "", limit); !errors.Is(err, want) {
			t.Errorf("a page of %d = %v, want %v", limit, err, want)
		}
	}
}
