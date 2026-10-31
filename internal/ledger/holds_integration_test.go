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
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// fundedDeposit creates a customer deposit (liability) with natural balance 100 and the clearing account
// (liability) its captures will go to.
func fundedDeposit(t *testing.T) (deposit, clearing ledger.Account) {
	t.Helper()
	cash := createAccount(t, ledger.KindAsset)
	deposit, clearing = createAccount(t, ledger.KindLiability), createAccount(t, ledger.KindLiability)
	mustPostRef(t, transfer(t, cash.ID, deposit.ID, ids.New().String()))
	return deposit, clearing
}

func inTx[T any](t *testing.T, fn func(tx postgres.Tx) (T, error)) (T, error) { //nolint:ireturn // T is a concrete type in each call
	t.Helper()
	var out T
	err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		var err error
		out, err = fn(tx)
		return err
	})
	return out, err
}

func placeHold(t *testing.T, account, counter ids.ID, units int64, expiresIn time.Duration) (ledger.Hold, error) {
	t.Helper()
	return inTx(t, func(tx postgres.Tx) (ledger.Hold, error) {
		return svc().PlaceHold(t.Context(), tx, ledger.PlaceHoldInput{
			LegalEntityID: entity, AccountID: account, CounterAccountID: counter,
			Amount: money.New(units, cop(t)), ExpiresAt: time.Now().Add(expiresIn),
		})
	})
}

type captured struct {
	hold ledger.Hold
	tx   ledger.Transaction
}

func captureInput(t *testing.T, hold ids.ID, units int64) ledger.CaptureInput {
	t.Helper()
	return ledger.CaptureInput{
		LegalEntityID: entity, HoldID: hold, Amount: money.New(units, cop(t)), BusinessDate: time.Now(), Actor: "tester", Channel: "test",
	}
}

func captureHold(t *testing.T, hold ids.ID, units int64) (captured, error) {
	t.Helper()
	return inTx(t, func(tx postgres.Tx) (captured, error) {
		h, posted, err := svc().CaptureHold(t.Context(), tx, captureInput(t, hold, units))
		return captured{h, posted}, err
	})
}

func releaseHold(t *testing.T, hold ids.ID) (ledger.Hold, error) {
	t.Helper()
	return inTx(t, func(tx postgres.Tx) (ledger.Hold, error) {
		return svc().ReleaseHold(t.Context(), tx, ledger.ReleaseInput{LegalEntityID: entity, HoldID: hold})
	})
}

func balanceOf(t *testing.T, account ids.ID) ledger.Balance {
	t.Helper()
	b, err := svc().Balance(t.Context(), testPool, account)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestAHoldReducesTheAvailable: a hold of 70 on a deposit of 100 leaves 30 available: neither a
// withdrawal of 100 nor another hold of 31 fits, and one of 30 does. The hold adds to pending_debits (the
// side that reduces a liability).
func TestAHoldReducesTheAvailable(t *testing.T) {
	deposit, clearing := fundedDeposit(t)

	hold, err := placeHold(t, deposit.ID, clearing.ID, 70, time.Hour)

	if err != nil || hold.Status() != ledger.HoldPending || hold.Amount().Units() != 70 {
		t.Fatalf("PlaceHold = %+v, %v", hold, err)
	}
	if b := balanceOf(t, deposit.ID); b.PendingDebits != 70 || b.PendingCredits != 0 || b.Posted != -100 {
		t.Fatalf("balance with the hold = %+v, want 70 pending debits and posted untouched", b)
	}
	if _, _, err := postRef(t, transfer(t, deposit.ID, clearing.ID, ids.New().String())); !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("withdrawing 100 with 70 held = %v, want ErrInsufficientFunds", err)
	}
	if _, err := placeHold(t, deposit.ID, clearing.ID, 31, time.Hour); !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("another hold of 31 = %v, want ErrInsufficientFunds", err)
	}
	if _, err := placeHold(t, deposit.ID, clearing.ID, 30, time.Hour); err != nil {
		t.Fatalf("another hold of 30 = %v, it fits exactly", err)
	}
}

// TestAHoldOnAnAssetUsesPendingCredits: on a debit-normal account money leaves through the credit side,
// so the hold adds to pending_credits.
func TestAHoldOnAnAssetUsesPendingCredits(t *testing.T) {
	cash, funding := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	mustPostRef(t, transfer(t, cash.ID, funding.ID, ids.New().String())) // cash with 100

	if _, err := placeHold(t, cash.ID, funding.ID, 60, time.Hour); err != nil {
		t.Fatal(err)
	}
	if b := balanceOf(t, cash.ID); b.PendingCredits != 60 || b.PendingDebits != 0 {
		t.Fatalf("balance = %+v, want 60 pending credits", b)
	}
}

// TestCaptureFromAnAssetCreditsIt: the capture takes what is captured out of an asset through the credit
// side and releases its pending_credits.
func TestCaptureFromAnAssetCreditsIt(t *testing.T) {
	cash, funding := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	mustPostRef(t, transfer(t, cash.ID, funding.ID, ids.New().String())) // cash with 100
	hold, err := placeHold(t, cash.ID, funding.ID, 60, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := captureHold(t, hold.ID(), 60); err != nil {
		t.Fatal(err)
	}
	if b := balanceOf(t, cash.ID); b.Posted != 40 || b.PendingCredits != 0 {
		t.Fatalf("cash after capturing 60 = %+v, want posted 40 and nothing pending", b)
	}
	if b := balanceOf(t, funding.ID); b.Posted != -40 {
		t.Fatalf("counterpart = %+v, want -40", b)
	}
}

// TestCaptureIntoAnAsyncCounterparty: the counterpart may be async; the capture neither locks it nor
// touches its balance, which the aggregator will carry, but its entry is posted.
func TestCaptureIntoAnAsyncCounterparty(t *testing.T) {
	deposit, _ := fundedDeposit(t)
	clearing := asyncAccount(t, ledger.KindLiability)
	hold, err := placeHold(t, deposit.ID, clearing.ID, 30, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	got, err := captureHold(t, hold.ID(), 30)
	if err != nil {
		t.Fatal(err)
	}
	if b := balanceOf(t, deposit.ID); b.Posted != -70 || b.PendingDebits != 0 {
		t.Fatalf("deposit = %+v, want posted -70 and nothing pending", b)
	}
	if b := balanceOf(t, clearing.ID); b.Posted != 0 {
		t.Fatalf("materialized balance of the async counterpart = %+v, want untouched", b)
	}
	checkCaptureTransaction(t, hold, got.tx)
}

// TestARejectedCaptureWritesNothing: even if the caller commits the transaction after the error, a
// capture the posting rejects leaves neither the pending amount released nor the hold closed.
func TestARejectedCaptureWritesNothing(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	hold, err := placeHold(t, deposit.ID, clearing.ID, 50, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	in := captureInput(t, hold.ID(), 50)
	in.Actor = ""
	var captureErr error
	err = postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		_, _, captureErr = svc().CaptureHold(t.Context(), tx, in)
		return nil
	})
	if err != nil || !errors.Is(captureErr, ledger.ErrInvalidPosting) {
		t.Fatalf("capture without an actor = %v (commit: %v), want ErrInvalidPosting", captureErr, err)
	}

	if b := balanceOf(t, deposit.ID); b.PendingDebits != 50 || b.Posted != -100 {
		t.Fatalf("balance = %+v, want the hold still held and posted untouched", b)
	}
	if _, err := captureHold(t, hold.ID(), 50); err != nil {
		t.Fatalf("the hold must stay pending: %v", err)
	}
}

// TestCapturePostsAndReleasesTheRest: a partial capture posts what is captured from the source to the
// counterpart, releases the rest and leaves the hold captured with its transaction.
func TestCapturePostsAndReleasesTheRest(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	hold, err := placeHold(t, deposit.ID, clearing.ID, 70, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	got, err := captureHold(t, hold.ID(), 40)

	if err != nil || got.hold.Status() != ledger.HoldCaptured || got.hold.Captured().Units() != 40 {
		t.Fatalf("CaptureHold = %+v, %v", got.hold, err)
	}
	if b := balanceOf(t, deposit.ID); b.Posted != -60 || b.PendingDebits != 0 {
		t.Fatalf("deposit after capturing 40 = %+v, want posted -60 and nothing pending", b)
	}
	if b := balanceOf(t, clearing.ID); b.Posted != -40 {
		t.Fatalf("clearing = %+v, want -40", b)
	}
	checkCaptureTransaction(t, hold, got.tx)
	if _, err := captureHold(t, hold.ID(), 10); !errors.Is(err, ledger.ErrHoldNotPending) {
		t.Fatalf("capturing again = %v, want ErrHoldNotPending", err)
	}
}

// checkCaptureTransaction checks that the capture's transaction is stored with the hold's source and
// reference.
func checkCaptureTransaction(t *testing.T, hold ledger.Hold, tx ledger.Transaction) {
	t.Helper()
	stored, err := svc().Transaction(t.Context(), testPool, entity, tx.ID)
	if err != nil || len(stored.Entries) != 2 || stored.Source != "ledger.hold" || stored.ExternalRef != hold.ID().String() {
		t.Fatalf("capture transaction = %+v, %v", stored.TransactionRecord, err)
	}
}

func TestReleaseRestoresTheAvailable(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	hold, err := placeHold(t, deposit.ID, clearing.ID, 70, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	released, err := releaseHold(t, hold.ID())

	if err != nil || released.Status() != ledger.HoldReleased {
		t.Fatalf("ReleaseHold = %+v, %v", released, err)
	}
	if b := balanceOf(t, deposit.ID); b.PendingDebits != 0 || b.Posted != -100 {
		t.Fatalf("balance after releasing = %+v", b)
	}
	if _, err := captureHold(t, hold.ID(), 10); !errors.Is(err, ledger.ErrHoldNotPending) {
		t.Fatalf("capturing a released one = %v, want ErrHoldNotPending", err)
	}
}

// TestCaptureRejectionsLeaveTheHoldPending: a capture for too much or after the expiry touches nothing.
// The expired hold is inserted directly, without waiting for it to expire, and with its pending amount,
// as PlaceHold leaves it: that way the database still adds up for other tests' expiry.
func TestCaptureRejectionsLeaveTheHoldPending(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	hold, err := placeHold(t, deposit.ID, clearing.ID, 70, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := captureHold(t, hold.ID(), 71); !errors.Is(err, ledger.ErrAmountExceedsHold) {
		t.Fatalf("capturing too much = %v, want ErrAmountExceedsHold", err)
	}
	expired := ids.New()
	if _, err := testPool.Exec(t.Context(), `
		WITH h AS (
			INSERT INTO ledger.holds (id, legal_entity_id, account_id, counter_account_id, currency, amount, expires_at)
			VALUES ($1, $2, $3, $4, 'COP', 10, clock_timestamp() - interval '1 second')
			RETURNING account_id, amount)
		UPDATE ledger.balances b SET pending_debits = b.pending_debits + h.amount, version = b.version + 1
		FROM h WHERE b.account_id = h.account_id`,
		expired, entity, deposit.ID, clearing.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := captureHold(t, expired, 10); !errors.Is(err, ledger.ErrHoldExpired) {
		t.Fatalf("capturing an expired one = %v, want ErrHoldExpired", err)
	}
	if b := balanceOf(t, deposit.ID); b.PendingDebits != 80 || b.Posted != -100 {
		t.Fatalf("balance = %+v, want both holds pending and posted untouched", b)
	}
}

func TestPlaceHoldRejects(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	async := asyncAccount(t, ledger.KindLiability)
	cases := map[string]struct {
		account, counter ids.ID
		want             error
	}{
		"nonexistent account":      {ids.New(), clearing.ID, ledger.ErrAccountNotFound},
		"nonexistent counterpart":  {deposit.ID, ids.New(), ledger.ErrAccountNotFound},
		"async account":            {async.ID, clearing.ID, ledger.ErrInvalidHold},
		"the same account":         {deposit.ID, deposit.ID, ledger.ErrInvalidHold},
		"another entity's account": {accountOf(t, ids.New(), ledger.KindLiability).ID, clearing.ID, ledger.ErrAccountNotFound},
	}
	for name, tc := range cases {
		if _, err := placeHold(t, tc.account, tc.counter, 10, time.Hour); !errors.Is(err, tc.want) {
			t.Errorf("%s = %v, want %v", name, err, tc.want)
		}
	}
	if _, err := placeHold(t, deposit.ID, clearing.ID, 10, -time.Hour); !errors.Is(err, ledger.ErrInvalidHold) {
		t.Errorf("already expired hold = %v, want ErrInvalidHold", err)
	}
	if b := balanceOf(t, deposit.ID); b.PendingDebits != 0 {
		t.Fatalf("pending = %d, want 0: no rejected hold holds funds", b.PendingDebits)
	}
}

// TestTheHoldSourceIsReserved: an arbitrary posting cannot take the reference of a hold's capture
// (source ledger.hold) or any other ledger.* source.
func TestTheHoldSourceIsReserved(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	hold, err := placeHold(t, deposit.ID, clearing.ID, 50, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	squatter := transfer(t, clearing.ID, deposit.ID, hold.ID().String())
	squatter.Source = "ledger.hold"

	if _, _, err := postRef(t, squatter); !errors.Is(err, ledger.ErrInvalidPosting) {
		t.Fatalf("posting with source ledger.hold = %v, want ErrInvalidPosting", err)
	}
	if _, err := captureHold(t, hold.ID(), 50); err != nil {
		t.Fatalf("the capture must go through: %v", err)
	}
}

func TestHoldsOfAnotherEntityAreNotFound(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	hold, err := placeHold(t, deposit.ID, clearing.ID, 50, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, err = inTx(t, func(tx postgres.Tx) (ledger.Hold, error) {
		return svc().ReleaseHold(t.Context(), tx, ledger.ReleaseInput{LegalEntityID: ids.New(), HoldID: hold.ID()})
	})
	if !errors.Is(err, ledger.ErrHoldNotFound) {
		t.Fatalf("releasing another entity's hold = %v, want ErrHoldNotFound", err)
	}
}

// TestTheHoldsTableEnforcesItsInvariants: even with direct SQL and the application role, a finished hold
// does not change, neither do its amount and accounts, it is not deleted, it is not born finished, and
// only a captured one has a transaction and a captured amount, different from another hold's.
func TestTheHoldsTableEnforcesItsInvariants(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	released, err := placeHold(t, deposit.ID, clearing.ID, 10, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := releaseHold(t, released.ID()); err != nil {
		t.Fatal(err)
	}
	pending, err := placeHold(t, deposit.ID, clearing.ID, 10, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	captured, err := placeHold(t, deposit.ID, clearing.ID, 10, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := captureHold(t, captured.ID(), 10); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		sql      string
		hold     ids.ID
		sqlState string
	}{
		"reopen a finished hold":                  {`UPDATE ledger.holds SET status = 'pending' WHERE id = $1`, released.ID(), "2F003"},
		"change the amount":                       {`UPDATE ledger.holds SET amount = 1 WHERE id = $1`, released.ID(), "42501"},
		"delete":                                  {`DELETE FROM ledger.holds WHERE id = $1`, released.ID(), "42501"},
		"release with a transaction":              {`UPDATE ledger.holds SET status = 'released', capture_tx_id = gen_random_uuid() WHERE id = $1`, pending.ID(), "23514"},
		"release with a captured amount":          {`UPDATE ledger.holds SET status = 'released', captured_amount = 1 WHERE id = $1`, pending.ID(), "23514"},
		"capture without an amount":               {`UPDATE ledger.holds SET status = 'captured', capture_tx_id = gen_random_uuid() WHERE id = $1`, pending.ID(), "23514"},
		"capture with another hold's transaction": {`UPDATE ledger.holds SET status = 'captured', captured_amount = 1, capture_tx_id = (SELECT h.capture_tx_id FROM ledger.holds h WHERE h.account_id = holds.account_id AND h.status = 'captured') WHERE id = $1`, pending.ID(), "23505"},
		"insert an already captured hold":         {`INSERT INTO ledger.holds (id, legal_entity_id, account_id, counter_account_id, currency, amount, expires_at, status) SELECT gen_random_uuid(), legal_entity_id, account_id, counter_account_id, currency, amount, expires_at, 'captured' FROM ledger.holds WHERE id = $1`, pending.ID(), "42501"},
	}
	for name, tc := range cases {
		_, err := testPool.Exec(t.Context(), tc.sql, tc.hold)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != tc.sqlState {
			t.Errorf("%s = %v, want SQLSTATE %s", name, err, tc.sqlState)
		}
	}
}

// TestASecondCaptureWaitsForTheFirst (AB-3): the second capture waits for the lock on the hold the first
// one holds and, when that one commits, sees the hold captured: ErrHoldNotPending, without touching the
// balances.
func TestASecondCaptureWaitsForTheFirst(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	hold, err := placeHold(t, deposit.ID, clearing.ID, 50, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pid, commitFirst := captureUncommitted(t, hold.ID())
	second := make(chan error, 1)
	go func() { _, err := captureHold(t, hold.ID(), 50); second <- err }()
	waitForBlockedBy(t, pid)

	if err := commitFirst(); err != nil {
		t.Fatal(err)
	}
	if err := <-second; !errors.Is(err, ledger.ErrHoldNotPending) {
		t.Fatalf("the second capture = %v, want ErrHoldNotPending", err)
	}
	if b := balanceOf(t, deposit.ID); b.Posted != -50 || b.PendingDebits != 0 {
		t.Fatalf("balance = %+v, want posted -50 and nothing pending: it was posted once", b)
	}
}

// captureUncommitted captures the whole hold in a transaction that stays open and returns its session's
// pid and the function that commits it.
func captureUncommitted(t *testing.T, hold ids.ID) (int32, func() error) {
	t.Helper()
	pids, done, release := make(chan int32, 1), make(chan error, 1), make(chan struct{})
	t.Cleanup(sync.OnceFunc(func() { close(release) }))
	go func() {
		done <- postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
			if _, _, err := svc().CaptureHold(t.Context(), tx, captureInput(t, hold, 50)); err != nil {
				return err
			}
			var pid int32
			if err := tx.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return err
			}
			pids <- pid
			<-release
			return nil
		})
	}()
	select {
	case pid := <-pids:
		return pid, func() error { release <- struct{}{}; return <-done }
	case err := <-done:
		t.Fatalf("the first capture failed: %v", err)
		return 0, nil
	}
}
