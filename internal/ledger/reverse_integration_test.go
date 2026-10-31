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

const reason = "typing error"

// reversalDay is the reversals' business date: two days after the original, so that a reversal with the
// wrong value date stands out.
var reversalDay = time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 2)

// tryReverse reverses in in its own transaction. It does not call t.Fatal: it works from other goroutines.
func tryReverse(t *testing.T, in ledger.ReverseInput) (ledger.Transaction, error) {
	t.Helper()
	var reversal ledger.Transaction
	err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		var err error
		reversal, err = svc().Reverse(t.Context(), tx, in)
		return err
	})
	return reversal, err
}

func reverse(t *testing.T, original ids.ID) (ledger.Transaction, error) {
	t.Helper()
	return tryReverse(t, ledger.ReverseInput{
		LegalEntityID: entity, TransactionID: original, Reason: reason, Actor: "operator", BusinessDate: reversalDay,
	})
}

// TestReverseUndoesATransaction: the reversal inverts the sign of each entry, points to the original and
// leaves the balances as they were; read from the database it is the same one Reverse returned.
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
		t.Fatalf("balances after the reversal = %d and %d, want 0 and 0", pc, pd)
	}
	for _, posted := range []ledger.Transaction{original, reversal} { // without metadata and with metadata
		stored, err := svc().Transaction(t.Context(), testPool, entity, posted.ID)
		if err != nil || !reflect.DeepEqual(stored, posted) {
			t.Fatalf("read from the database = %+v (%v), want the same one that was posted %+v", stored, err, posted)
		}
	}
}

// checkReversal checks that reversal points to original, stores the reason, has the entries want and
// carries the original's value date and the reversal's business date.
func checkReversal(t *testing.T, original, reversal ledger.Transaction, want []ledger.EntryInput) {
	t.Helper()
	if reversal.ReversesID != original.ID || reversal.Metadata["reason"] != reason || !slices.Equal(reversal.Entries, want) {
		t.Errorf("the reversal = %+v, it must point to %s, store the reason and have the entries %+v", reversal, original.ID, want)
	}
	if !reversal.ValueDate.Equal(original.ValueDate) || !reversal.BusinessDate.Equal(reversalDay) {
		t.Errorf("reversal dates = value %v, business %v; want the original's value date %v and business %v",
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
		t.Fatalf("second reversal = %s, %v; want ErrAlreadyReversed with the first one %s", again.ID, err, first.ID)
	}
	if pc := postedOf(t, cash.ID); pc != 0 {
		t.Fatalf("cash balance = %d, want 0: the second reversal moves no money", pc)
	}
}

// TestConcurrentReversalsOfAsyncAccountsReverseOnce: with no balances to lock (async accounts), the
// concurrent reversals of a transaction conflict only in the database; one goes through and the rest get
// ErrAlreadyReversed, never a 23505.
func TestConcurrentReversalsOfAsyncAccountsReverseOnce(t *testing.T) {
	a, b := asyncAccount(t, ledger.KindAsset), asyncAccount(t, ledger.KindLiability)
	original, _ := mustPostRef(t, transfer(t, a.ID, b.ID, ids.New().String()))

	errs := concurrently(t, 8, func(int) error { _, err := reverse(t, original.ID); return err })

	reversed := 0
	for _, err := range errs {
		switch {
		case err == nil:
			reversed++
		case !errors.Is(err, ledger.ErrAlreadyReversed):
			t.Errorf("concurrent reversal with an unexpected error: %v", err)
		}
	}
	if reversed != 1 {
		t.Fatalf("%d reversals went through, want 1", reversed)
	}
}

func asyncAccount(t *testing.T, kind ledger.Kind) ledger.Account {
	t.Helper()
	a, err := tryCreateAccount(t, ledger.CreateAccountInput{
		LegalEntityID: entity, Code: "acct-" + ids.New().String(), Kind: kind, Currency: cop(t), Mode: ledger.BalanceModeAsync,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestOnlyReversalsUseTheReversalSource: an arbitrary posting, including a manual one through the API,
// cannot take the place of another transaction's reversal or mark itself as a reversal on its own.
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
		"reversal source": squatter, "hand-set ReversesID": selfReversal, "reversal with another reference": otherRef,
	} {
		if _, _, err := postRef(t, in); !errors.Is(err, ledger.ErrInvalidPosting) {
			t.Errorf("%s = %v, want ErrInvalidPosting", name, err)
		}
	}
	if _, err := reverse(t, original.ID); err != nil {
		t.Fatalf("the original must be reversible: %v", err)
	}
}

// TestReversingASpentDepositRespectsTheLimits: reversing a deposit whose money already left would leave
// the account negative: ErrInsufficientFunds, like any posting, and the balances do not change.
func TestReversingASpentDepositRespectsTheLimits(t *testing.T) {
	cash, deposit, clearing := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability), createAccount(t, ledger.KindLiability)
	credited, _ := mustPostRef(t, transfer(t, cash.ID, deposit.ID, ids.New().String()))
	mustPostRef(t, transfer(t, deposit.ID, clearing.ID, ids.New().String()))

	if _, err := reverse(t, credited.ID); !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("reversing a spent deposit = %v, want ErrInsufficientFunds", err)
	}
	if pc, pd := postedOf(t, cash.ID), postedOf(t, deposit.ID); pc != 100 || pd != 0 {
		t.Fatalf("balances = %d and %d, want 100 and 0: the rejected reversal moves no money", pc, pd)
	}
}

// TestAReversalCanBeReversed: reversing a reversal applies the original again.
func TestAReversalCanBeReversed(t *testing.T) {
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	original, _ := mustPostRef(t, transfer(t, cash.ID, deposit.ID, ids.New().String()))
	reversal, err := reverse(t, original.ID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := reverse(t, reversal.ID); err != nil || postedOf(t, cash.ID) != 100 {
		t.Fatalf("reversing the reversal = %v, cash %d; want cash at 100", err, postedOf(t, cash.ID))
	}
}

func TestReverseRejects(t *testing.T) {
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	original, _ := mustPostRef(t, transfer(t, cash.ID, deposit.ID, ids.New().String()))
	cases := map[string]struct {
		in   ledger.ReverseInput
		want error
	}{
		"nonexistent transaction": {ledger.ReverseInput{LegalEntityID: entity, TransactionID: ids.New(), Reason: "r", Actor: "a", BusinessDate: time.Now()}, ledger.ErrTransactionNotFound},
		"of another entity":       {ledger.ReverseInput{LegalEntityID: ids.New(), TransactionID: original.ID, Reason: "r", Actor: "a", BusinessDate: time.Now()}, ledger.ErrTransactionNotFound},
		"no reason":               {ledger.ReverseInput{LegalEntityID: entity, TransactionID: original.ID, Actor: "a", BusinessDate: time.Now()}, ledger.ErrInvalidPosting},
		"no actor":                {ledger.ReverseInput{LegalEntityID: entity, TransactionID: original.ID, Reason: "r", BusinessDate: time.Now()}, ledger.ErrInvalidPosting},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := tryReverse(t, tc.in); !errors.Is(err, tc.want) {
				t.Fatalf("= %v, want %v", err, tc.want)
			}
		})
	}
	if pc := postedOf(t, cash.ID); pc != 100 {
		t.Fatalf("cash balance = %d, want 100: no rejected reversal moves money", pc)
	}
}

// TestAReversalRefTakenBySQLFailsClosed: if someone takes the reversal's reference through direct SQL
// (the database does not prevent it), Reverse does not consider the transaction reversed.
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
		t.Fatalf("= %v; with the reference taken by another transaction, Reverse must fail without considering it reversed", err)
	}
}
