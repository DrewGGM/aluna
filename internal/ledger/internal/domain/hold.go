package domain

import (
	"errors"
	"fmt"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
)

var (
	// ErrInvalidHold means a hold or a capture with invalid data: a non-positive amount, no currency, no
	// identifier, no account, no creation instant, one that expires on creation or, when restoring it, a
	// row that breaks the hold invariants.
	ErrInvalidHold = errors.New("invalid hold")
	// ErrHoldNotPending means an operation on a hold that already finished.
	ErrHoldNotPending = errors.New("the hold is not pending")
	// ErrAmountExceedsHold means a capture for more than what is held.
	ErrAmountExceedsHold = errors.New("the capture exceeds the hold")
	// ErrHoldExpired means a capture of a pending hold after its expiry.
	ErrHoldExpired = errors.New("the hold expired")
	// ErrHoldNotDue means an attempt to expire a hold before its expiry.
	ErrHoldNotDue = errors.New("the hold is not due yet")
)

// HoldStatus is a hold's status (FR-6): it starts pending and ends in exactly one of the others.
type HoldStatus string

// Hold statuses.
const (
	HoldPending  HoldStatus = "pending"
	HoldCaptured HoldStatus = "captured"
	HoldReleased HoldStatus = "released"
	HoldExpired  HoldStatus = "expired"
)

// Hold is a two-phase reservation of funds on an account: while pending it reduces the available
// balance; capturing it posts it (fully or partially, releasing the rest), and releasing or expiring it
// returns the available balance. It is immutable: each transition returns a new hold.
type Hold struct {
	id        ids.ID
	accountID ids.ID
	amount    money.Amount
	captured  money.Amount
	status    HoldStatus
	expiresAt time.Time
}

// NewHoldInput is the data for a new hold. Now is the creation instant, given by the use case's clock:
// the domain does not read the time.
type NewHoldInput struct {
	ID, AccountID ids.ID
	Amount        money.Amount
	ExpiresAt     time.Time
	Now           time.Time
}

// NewHold creates a pending hold. The amount is positive and it expires after Now.
func NewHold(in NewHoldInput) (Hold, error) {
	if err := checkIdentity(in.ID, in.AccountID, in.Amount); err != nil {
		return Hold{}, err
	}
	if in.Now.IsZero() || !in.ExpiresAt.After(in.Now) {
		return Hold{}, fmt.Errorf("%w: expires %v, not after its creation %v", ErrInvalidHold, in.ExpiresAt, in.Now)
	}
	return Hold{
		id: in.ID, accountID: in.AccountID, amount: in.Amount, captured: money.New(0, in.Amount.Currency()),
		status: HoldPending, expiresAt: in.ExpiresAt,
	}, nil
}

// HoldSnapshot is a hold as stored: what is read from the database to restore it.
type HoldSnapshot struct {
	ID, AccountID    ids.ID
	Amount, Captured money.Amount
	Status           HoldStatus
	ExpiresAt        time.Time
}

// RestoreHold restores a stored hold, in any status, including a pending one already past its expiry
// (so that Capture gives ErrHoldExpired and Expire closes it). It rejects a row that breaks the
// invariants: a known status, the captured amount in the hold's currency and between zero and the
// amount, and non-zero only if captured.
func RestoreHold(s HoldSnapshot) (Hold, error) {
	if err := checkIdentity(s.ID, s.AccountID, s.Amount); err != nil {
		return Hold{}, err
	}
	if err := checkCaptured(s); err != nil {
		return Hold{}, err
	}
	if s.ExpiresAt.IsZero() {
		return Hold{}, fmt.Errorf("%w: no expiry", ErrInvalidHold)
	}
	return Hold{
		id: s.ID, accountID: s.AccountID, amount: s.Amount, captured: s.Captured, status: s.Status, expiresAt: s.ExpiresAt,
	}, nil
}

// checkCaptured validates the stored status and the captured amount against it.
func checkCaptured(s HoldSnapshot) error {
	switch s.Status {
	case HoldPending, HoldCaptured, HoldReleased, HoldExpired:
	default:
		return fmt.Errorf("%w: status %q", ErrInvalidHold, s.Status)
	}
	if s.Captured.Currency() != s.Amount.Currency() || s.Captured.Sign() < 0 || s.Captured.Units() > s.Amount.Units() {
		return fmt.Errorf("%w: captured %d %q of %d %q", ErrInvalidHold, s.Captured.Units(), s.Captured.Currency().Code(),
			s.Amount.Units(), s.Amount.Currency().Code())
	}
	if (s.Captured.Sign() > 0) != (s.Status == HoldCaptured) {
		return fmt.Errorf("%w: %s with %d captured", ErrInvalidHold, s.Status, s.Captured.Units())
	}
	return nil
}

// checkIdentity validates what every hold has: an identifier, an account and a positive amount with a
// currency.
func checkIdentity(id, accountID ids.ID, amount money.Amount) error {
	switch {
	case id == (ids.ID{}) || accountID == (ids.ID{}):
		return fmt.Errorf("%w: no identifier or no account", ErrInvalidHold)
	case amount.Sign() <= 0 || amount.Currency() == (money.Currency{}):
		return fmt.Errorf("%w: amount %d %q", ErrInvalidHold, amount.Units(), amount.Currency().Code())
	}
	return nil
}

// ID is the hold's identifier.
func (h Hold) ID() ids.ID { return h.id }

// AccountID is the account whose funds the hold reserves.
func (h Hold) AccountID() ids.ID { return h.accountID }

// Amount is what is held, positive.
func (h Hold) Amount() money.Amount { return h.amount }

// Captured is what was posted on capture; zero if it was not captured.
func (h Hold) Captured() money.Amount { return h.captured }

// Status is the hold's status.
func (h Hold) Status() HoldStatus { return h.status }

// ExpiresAt is the expiry: it can be captured up to that instant inclusive, and the job expires it when
// expires_at < now.
func (h Hold) ExpiresAt() time.Time { return h.expiresAt }

// CaptureResult is what a capture leaves: Captured is posted and Released returns to the available
// balance.
type CaptureResult struct {
	Captured, Released money.Amount
}

// Capture posts units of the hold at now, up to its expiry inclusive. A partial capture releases the rest
// (FR-6). The status is checked before the expiry: a hold that already expired answers
// ErrHoldNotPending, not ErrHoldExpired.
func (h Hold) Capture(units int64, now time.Time) (Hold, CaptureResult, error) {
	if err := h.requirePending(); err != nil {
		return Hold{}, CaptureResult{}, err
	}
	switch {
	case now.After(h.expiresAt):
		return Hold{}, CaptureResult{}, fmt.Errorf("%w: expired %v", ErrHoldExpired, h.expiresAt)
	case units <= 0:
		return Hold{}, CaptureResult{}, fmt.Errorf("%w: capture of %d", ErrInvalidHold, units)
	case units > h.amount.Units():
		return Hold{}, CaptureResult{}, fmt.Errorf("%w: %d of %d", ErrAmountExceedsHold, units, h.amount.Units())
	}
	cur := h.amount.Currency()
	captured := money.New(units, cur)
	next := h
	next.status, next.captured = HoldCaptured, captured
	return next, CaptureResult{Captured: captured, Released: money.New(h.amount.Units()-units, cur)}, nil
}

// Release releases the whole hold: the available balance returns.
func (h Hold) Release() (Hold, error) {
	if err := h.requirePending(); err != nil {
		return Hold{}, err
	}
	next := h
	next.status = HoldReleased
	return next, nil
}

// Expire closes the hold that is due at now (expires_at < now): the available balance returns.
func (h Hold) Expire(now time.Time) (Hold, error) {
	if err := h.requirePending(); err != nil {
		return Hold{}, err
	}
	if !now.After(h.expiresAt) {
		return Hold{}, fmt.Errorf("%w: expires %v", ErrHoldNotDue, h.expiresAt)
	}
	next := h
	next.status = HoldExpired
	return next, nil
}

func (h Hold) requirePending() error {
	if h.status != HoldPending {
		return fmt.Errorf("%w: it is %s", ErrHoldNotPending, h.status)
	}
	return nil
}
