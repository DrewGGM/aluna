//go:build integration

package ledger_test

import (
	"errors"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// afterTheHolds is a now at which these tests' one-hour holds are already due.
func afterTheHolds() time.Time { return time.Now().Add(2 * time.Hour) }

func expireDue(t *testing.T, now time.Time, batch int) int {
	t.Helper()
	n, err := svc().ExpireDueHolds(t.Context(), postgres.NewUnitOfWork(testPool), now, batch)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func mustPlaceHold(t *testing.T, account, counter ids.ID, units int64, expiresIn time.Duration) ids.ID {
	t.Helper()
	hold, err := placeHold(t, account, counter, units, expiresIn)
	if err != nil {
		t.Fatal(err)
	}
	return hold.ID()
}

func statusOf(t *testing.T, hold ids.ID) ledger.HoldStatus {
	t.Helper()
	var status ledger.HoldStatus
	if err := testPool.QueryRow(t.Context(), `SELECT status FROM ledger.holds WHERE id = $1`, hold).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// TestExpireDueHoldsReleasesThePending: the due holds, on a liability and on an asset, end expired and
// their amount leaves the pending balance on the side where it was held; one that expires later stays
// pending. With batches of one, ExpireDueHolds keeps going until the due ones are gone. The database is
// shared by the whole package: the total also counts pending holds left by other tests.
func TestExpireDueHoldsReleasesThePending(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	cash, funding := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	mustPostRef(t, transfer(t, cash.ID, funding.ID, ids.New().String()))
	due := []ids.ID{
		mustPlaceHold(t, deposit.ID, clearing.ID, 30, time.Hour), mustPlaceHold(t, deposit.ID, clearing.ID, 20, time.Hour),
		mustPlaceHold(t, cash.ID, funding.ID, 60, time.Hour),
	}
	later := mustPlaceHold(t, deposit.ID, clearing.ID, 10, 3*time.Hour)

	if n := expireDue(t, afterTheHolds(), 1); n < len(due) {
		t.Fatalf("expired %d holds, want at least %d", n, len(due))
	}

	for _, id := range due {
		if s := statusOf(t, id); s != ledger.HoldExpired {
			t.Errorf("hold %s = %s, want expired", id, s)
		}
	}
	if s := statusOf(t, later); s != ledger.HoldPending {
		t.Errorf("the hold that expires later = %s, want pending", s)
	}
	if b := balanceOf(t, deposit.ID); b.PendingDebits != 10 || b.Posted != -100 {
		t.Errorf("deposit = %+v, want 10 pending (the hold not yet due) and posted untouched", b)
	}
	if b := balanceOf(t, cash.ID); b.PendingCredits != 0 || b.Posted != 100 {
		t.Errorf("cash = %+v, want nothing pending and posted untouched", b)
	}
}

// TestABatchReleasesEveryHoldOfAnAccount: two holds of the same account in one batch together release
// the whole pending amount: they add up, they do not overwrite each other.
func TestABatchReleasesEveryHoldOfAnAccount(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	mustPlaceHold(t, deposit.ID, clearing.ID, 30, time.Hour)
	mustPlaceHold(t, deposit.ID, clearing.ID, 20, time.Hour)

	expireDue(t, afterTheHolds(), 100)

	if b := balanceOf(t, deposit.ID); b.PendingDebits != 0 {
		t.Fatalf("deposit = %+v, want nothing pending after expiring its two holds", b)
	}
}

// TestAHoldBeingCapturedIsNotExpired: the expiry skips the hold a capture has locked, without waiting for
// it; the capture commits and the hold ends captured.
func TestAHoldBeingCapturedIsNotExpired(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	hold := mustPlaceHold(t, deposit.ID, clearing.ID, 50, time.Hour)
	_, commitCapture := captureUncommitted(t, hold)

	expireDue(t, afterTheHolds(), 10)

	if err := commitCapture(); err != nil {
		t.Fatalf("the capture must commit: %v", err)
	}
	if s := statusOf(t, hold); s != ledger.HoldCaptured {
		t.Fatalf("hold = %s, want captured", s)
	}
	if b := balanceOf(t, deposit.ID); b.Posted != -50 || b.PendingDebits != 0 {
		t.Fatalf("deposit = %+v, want posted -50 and nothing pending", b)
	}
}

// TestAnExpiredHoldIsNotCaptured: if the expiry locks the hold first, the capture finds it expired.
func TestAnExpiredHoldIsNotCaptured(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	hold := mustPlaceHold(t, deposit.ID, clearing.ID, 50, time.Hour)
	expireDue(t, afterTheHolds(), 10)

	if _, err := captureHold(t, hold, 50); !errors.Is(err, ledger.ErrHoldNotPending) {
		t.Fatalf("capturing an expired hold = %v, want ErrHoldNotPending", err)
	}
	if b := balanceOf(t, deposit.ID); b.Posted != -100 || b.PendingDebits != 0 {
		t.Fatalf("deposit = %+v, want posted untouched and nothing pending", b)
	}
}

// TestAHoldIsNotDueAtItsExpiry: at the exact time of its expiry the hold can still be captured
// (Hold.Expire requires it to have passed), so the expiry neither takes it nor fails the batch.
func TestAHoldIsNotDueAtItsExpiry(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	hold, err := placeHold(t, deposit.ID, clearing.ID, 50, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	expireDue(t, hold.ExpiresAt(), 10)

	if s := statusOf(t, hold.ID()); s != ledger.HoldPending {
		t.Fatalf("hold at the time of its expiry = %s, want pending", s)
	}
}

func TestExpireDueHoldsRejectsAnEmptyBatch(t *testing.T) {
	if _, err := svc().ExpireDueHolds(t.Context(), postgres.NewUnitOfWork(testPool), afterTheHolds(), 0); err == nil {
		t.Fatal("a batch of 0 must fail: it would never advance")
	}
}
