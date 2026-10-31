package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger/internal/domain"
	"github.com/DrewGGM/aluna/internal/money"
)

// ErrHoldNotFound means a hold that does not exist or belongs to another legal entity.
var ErrHoldNotFound = errors.New("hold not found")

// Header of the transaction that posts a capture. The external reference is the hold: a hold is posted at
// most once.
const (
	holdSource             = "ledger.hold"
	holdCaptureCode        = "hold_capture"
	holdCaptureRuleVersion = "hold-capture.v1"
)

// Hold is a hold with its counterpart, the account that receives what is captured. It is read-only: its
// transitions are made by the use cases, which store them.
type Hold struct {
	hold             domain.Hold
	counterAccountID ids.ID
}

// RestoredHold builds a Hold read from the database.
func RestoredHold(h domain.Hold, counterAccountID ids.ID) Hold {
	return Hold{hold: h, counterAccountID: counterAccountID}
}

// ID is the hold's identifier.
func (h Hold) ID() ids.ID { return h.hold.ID() }

// AccountID is the account whose funds it reserves.
func (h Hold) AccountID() ids.ID { return h.hold.AccountID() }

// CounterAccountID is the account that receives what is captured.
func (h Hold) CounterAccountID() ids.ID { return h.counterAccountID }

// Amount is what is held.
func (h Hold) Amount() money.Amount { return h.hold.Amount() }

// Captured is what was posted on capture; zero if it was not captured.
func (h Hold) Captured() money.Amount { return h.hold.Captured() }

// Status is the hold's status.
func (h Hold) Status() domain.HoldStatus { return h.hold.Status() }

// ExpiresAt is the expiry.
func (h Hold) ExpiresAt() time.Time { return h.hold.ExpiresAt() }

func (h Hold) with(next domain.Hold) Hold {
	h.hold = next
	return h
}

// PlaceHoldInput asks to hold Amount of AccountID until ExpiresAt; what is captured goes to
// CounterAccountID.
type PlaceHoldInput struct {
	LegalEntityID, AccountID, CounterAccountID ids.ID
	Amount                                     money.Amount
	ExpiresAt                                  time.Time
}

// CaptureInput asks to post Amount, in the hold's currency, with business date BusinessDate.
type CaptureInput struct {
	LegalEntityID, HoldID ids.ID
	Amount                money.Amount
	BusinessDate          time.Time
	Actor, Channel        string
}

// ReleaseInput asks to release a hold.
type ReleaseInput struct {
	LegalEntityID, HoldID ids.ID
}

// HoldRecords is the persistence of the hold rows.
type HoldRecords interface {
	// DatabaseNow is the database time, the same one that sets posted_at.
	DatabaseNow(ctx context.Context) (time.Time, error)
	InsertHold(ctx context.Context, entity ids.ID, h Hold) error
	// LockHold locks (FOR UPDATE) and returns the entity's hold, or ErrHoldNotFound.
	LockHold(ctx context.Context, entity, id ids.ID) (Hold, error)
	// CloseHold stores the outcome of a hold that left pending; captureTxID is zero if it was not captured.
	CloseHold(ctx context.Context, h Hold, captureTxID ids.ID) error
}

// HoldStore is the persistence of the hold use cases: a capture posts with post.
type HoldStore interface {
	PostingStore
	HoldRecords
}

// PlaceHold reserves funds from the source account (FR-6): it checks the available balance as if the
// amount were leaving now, creates the pending hold and adds it to the pending side through which the
// account loses balance (pending_debits on a liability, pending_credits on an asset). The expiry is stored
// in UTC and with microseconds, the database's precision, so that the created hold equals the one read
// back. It emits hold.placed.
func PlaceHold(ctx context.Context, store HoldStore, events Events, in PlaceHoldInput) (Hold, error) {
	origin, err := holdOrigin(ctx, store, in)
	if err != nil {
		return Hold{}, err
	}
	outgoing := outgoingOf(origin, in.Amount)
	accounts := map[ids.ID]domain.Account{origin.ID: origin}
	if err := checkAvailable(ctx, store, accounts, []ids.ID{origin.ID}, map[ids.ID]money.Amount{origin.ID: outgoing}, pendingRelease{}); err != nil {
		return Hold{}, err
	}
	now, err := store.DatabaseNow(ctx)
	if err != nil {
		return Hold{}, err
	}
	placed, err := domain.NewHold(domain.NewHoldInput{
		ID: ids.New(), AccountID: origin.ID, Amount: in.Amount, ExpiresAt: in.ExpiresAt.UTC().Truncate(time.Microsecond), Now: now,
	})
	if err != nil {
		return Hold{}, err
	}
	h := RestoredHold(placed, in.CounterAccountID)
	if err := store.InsertHold(ctx, in.LegalEntityID, h); err != nil {
		return Hold{}, err
	}
	debits, credits := pendingOf(outgoing)
	if err := store.AddToBalance(ctx, origin.ID, BalanceDelta{PendingDebits: debits, PendingCredits: credits}); err != nil {
		return Hold{}, err
	}
	if err := events.Emit(ctx, holdEvent(typeHoldPlaced, in.LegalEntityID, h, ids.ID{})); err != nil {
		return Hold{}, err
	}
	return h, nil
}

// CaptureHold posts part or all of a hold (FR-6). It locks the hold before the balances (ADR-0004); the
// capture time is the database's after the lock, so between a capture and the expiry, whichever locks
// the hold first wins. What is captured is posted from the source to the counterpart and the whole
// pending amount is released in the posting's own write, after all its checks: a rejected capture leaves
// nothing written. It emits hold.captured.
func CaptureHold(ctx context.Context, store HoldStore, guard PeriodGuard, events Events, in CaptureInput) (Hold, Transaction, error) {
	h, err := store.LockHold(ctx, in.LegalEntityID, in.HoldID)
	if err != nil {
		return Hold{}, Transaction{}, err
	}
	now, err := store.DatabaseNow(ctx)
	if err != nil {
		return Hold{}, Transaction{}, err
	}
	next, result, err := h.hold.Capture(in.Amount.Units(), now)
	if err != nil {
		return Hold{}, Transaction{}, err
	}
	if in.Amount.Currency() != h.Amount().Currency() {
		return Hold{}, Transaction{}, fmt.Errorf("%w: capture in %s, hold in %s", ErrCurrencyMismatch, in.Amount.Currency().Code(), h.Amount().Currency().Code())
	}
	accounts, err := store.Accounts(ctx, []ids.ID{h.AccountID()})
	if err != nil {
		return Hold{}, Transaction{}, err
	}
	origin := accounts[h.AccountID()]
	debits, credits := pendingOf(outgoingOf(origin, h.Amount()))
	release := pendingRelease{account: origin.ID, debits: debits, credits: credits}
	posted, err := post(ctx, store, guard, captureInput(in, h, outgoingOf(origin, result.Captured)), release)
	if err != nil {
		return Hold{}, Transaction{}, err
	}
	captured := h.with(next)
	if err := store.CloseHold(ctx, captured, posted.ID); err != nil {
		return Hold{}, Transaction{}, err
	}
	// its event is hold.captured, with the transaction: the capture's posting does not emit transaction.posted
	if err := events.Emit(ctx, holdEvent(typeHoldCaptured, in.LegalEntityID, captured, posted.ID)); err != nil {
		return Hold{}, Transaction{}, err
	}
	return captured, posted, nil
}

// captureInput is a capture's posting: outgoing leaves the source and enters the counterpart.
func captureInput(in CaptureInput, h Hold, outgoing money.Amount) PostInput {
	incoming := money.New(-outgoing.Units(), outgoing.Currency()) // |outgoing| ≤ the hold amount: no overflow
	return PostInput{
		LegalEntityID: in.LegalEntityID, Code: holdCaptureCode, ValueDate: in.BusinessDate, BusinessDate: in.BusinessDate,
		Source: holdSource, ExternalRef: h.ID().String(), Actor: in.Actor, Channel: in.Channel, RuleVersion: holdCaptureRuleVersion,
		Entries: []domain.EntryInput{{AccountID: h.AccountID(), Amount: outgoing}, {AccountID: h.CounterAccountID(), Amount: incoming}},
	}
}

// ReleaseHold releases a pending hold: its amount leaves the pending balance and the available balance
// returns. It emits hold.released.
func ReleaseHold(ctx context.Context, store HoldStore, events Events, in ReleaseInput) (Hold, error) {
	h, err := store.LockHold(ctx, in.LegalEntityID, in.HoldID)
	if err != nil {
		return Hold{}, err
	}
	next, err := h.hold.Release()
	if err != nil {
		return Hold{}, err
	}
	accounts, err := store.Accounts(ctx, []ids.ID{h.AccountID()})
	if err != nil {
		return Hold{}, err
	}
	debits, credits := pendingOf(outgoingOf(accounts[h.AccountID()], h.Amount()))
	if err := store.AddToBalance(ctx, h.AccountID(), BalanceDelta{PendingDebits: -debits, PendingCredits: -credits}); err != nil {
		return Hold{}, err
	}
	released := h.with(next)
	if err := store.CloseHold(ctx, released, ids.ID{}); err != nil {
		return Hold{}, err
	}
	if err := events.Emit(ctx, holdEvent(typeHoldReleased, in.LegalEntityID, released, ids.ID{})); err != nil {
		return Hold{}, err
	}
	return released, nil
}

// holdOrigin loads the source and counterpart of a new hold, which must be different, of the entity and
// in the hold's currency; the source must also be sync, because only a sync account carries its pending
// balance. It returns the source. A stored hold is not revalidated: its FKs and holds_distinct_accounts
// guarantee it.
func holdOrigin(ctx context.Context, store PostingStore, in PlaceHoldInput) (domain.Account, error) {
	if in.AccountID == in.CounterAccountID {
		return domain.Account{}, fmt.Errorf("%w: source and counterpart are the same account", domain.ErrInvalidHold)
	}
	accounts, err := store.Accounts(ctx, []ids.ID{in.AccountID, in.CounterAccountID})
	if err != nil {
		return domain.Account{}, err
	}
	for _, id := range []ids.ID{in.AccountID, in.CounterAccountID} {
		a, ok := accounts[id]
		if !ok || a.LegalEntityID != in.LegalEntityID {
			return domain.Account{}, fmt.Errorf("%w: %s", ErrAccountNotFound, id)
		}
		if a.Currency != in.Amount.Currency() {
			return domain.Account{}, fmt.Errorf("%w: account %s in %s, hold in %s", ErrCurrencyMismatch, a.Code, a.Currency.Code(), in.Amount.Currency().Code())
		}
	}
	origin := accounts[in.AccountID]
	if origin.Mode != domain.BalanceModeSync {
		return domain.Account{}, fmt.Errorf("%w: account %s is async and carries no pending balance", domain.ErrInvalidHold, origin.Code)
	}
	return origin, nil
}

// outgoingOf is amount with the sign with which it leaves a: a credit on a debit-normal account, a debit
// on a credit-normal one.
func outgoingOf(a domain.Account, amount money.Amount) money.Amount {
	return money.New(-domain.NaturalBalance(a.Kind, amount.Units()), amount.Currency())
}

// pendingOf splits outgoing between the two pending balances, which are stored positive: a debit goes to
// pending_debits and a credit to pending_credits.
func pendingOf(outgoing money.Amount) (debits, credits int64) {
	if outgoing.Sign() > 0 {
		return outgoing.Units(), 0
	}
	return 0, -outgoing.Units()
}
