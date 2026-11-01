package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/payments/internal/domain"
	"github.com/DrewGGM/aluna/internal/payments/internal/domain/postingrules"
)

// ReasonRailRefused is why an order the rail did not take is rejected: it is not an ISO 20022 code, because the
// rail did not reject the payment, it never took it.
const ReasonRailRefused = "RAIL_REFUSED"

var (
	// ErrOrderNotFound means no order has that id or that end-to-end id.
	ErrOrderNotFound = errors.New("payment order not found")
	// ErrOutcomeConflict means the rail notified an order that already took another outcome: reconciliation
	// decides (FR-34).
	ErrOutcomeConflict = errors.New("the payment order already took another outcome")
)

// Longest rail reference and rejection reason (payments.orders).
const (
	maxRailRef = 255
	maxReason  = 255
)

// Sending is what moving an order to sent needs, within its transaction.
type Sending interface {
	// LockOrder locks and returns order id, or ErrOrderNotFound.
	LockOrder(ctx context.Context, id ids.ID) (Order, error)
	// Update stores where o stands and publishes it.
	Update(ctx context.Context, o Order) error
}

// StartSending moves order id to sent, before it is handed to its rail, and reports whether it did: an order
// that is not held was already sent, or never will be, and is not sent again.
func StartSending(ctx context.Context, p Sending, id ids.ID) (Order, bool, error) {
	o, err := p.LockOrder(ctx, id)
	if err != nil {
		return Order{}, false, err
	}
	if err := o.Send(); errors.Is(err, domain.ErrIllegalTransition) {
		return o, false, nil
	}
	return o, true, p.Update(ctx, o)
}

// Message is o's message to its rail, from its deposit account.
func Message(o Order) OutboundMessage {
	return OutboundMessage{
		EndToEndID: o.EndToEndID, InstrID: o.InstrID, TxID: o.TxID, Amount: o.Amount,
		Debtor: AccountRef{Number: o.AccountID.String()}, Creditor: o.Creditor,
	}
}

// ChargeAccounts are the ledger accounts an order's capture posts to besides the clearing account: its deposit
// account's, and those its product names for its fees and for the GMF.
type ChargeAccounts struct {
	Customer, FeeIncome, GMFPayable ids.ID
}

// Settling is what taking a rail's outcome needs, within its transaction.
type Settling interface {
	// LockByEndToEndID locks and returns the order with end-to-end id e2e, or ErrOrderNotFound.
	LockByEndToEndID(ctx context.Context, e2e string) (Order, error)
	// Accounts are o's charge accounts; one that o does not charge is the zero ID and is not looked up.
	Accounts(ctx context.Context, o Order) (ChargeAccounts, error)
	// Capture captures o's amount from its hold against its rail's clearing account, which releases the rest
	// of the hold, and posts charges, the entries of o's charges, unless there are none.
	Capture(ctx context.Context, o Order, charges []ledger.EntryInput) error
	// Release releases o's hold.
	Release(ctx context.Context, o Order) error
	// Update stores where o stands and publishes it.
	Update(ctx context.Context, o Order) error
}

// TakeOutcome applies the rail's outcome to its order (FR-31): accepted, the order settles and its hold is
// captured with its charges; rejected, it is rejected for the rail's reason and its hold released. An order
// no longer sent already took its outcome: the same one again changes nothing (TH-7), and another is
// ErrOutcomeConflict. An outcome that is neither, or whose reference or reason is not text of up to 255
// characters, is ErrInvalidNotification.
func TakeOutcome(ctx context.Context, p Settling, out RailOutcome) error {
	if err := checkOutcome(out); err != nil {
		return err
	}
	o, err := p.LockByEndToEndID(ctx, out.EndToEndID)
	if err != nil {
		return err
	}
	if o.Status != domain.StatusSent {
		return tookAlready(o, out)
	}
	o.RailRef = out.RailRef
	if out.Status == RailAccepted {
		err = settle(ctx, p, &o)
	} else {
		err = reject(ctx, p, &o, out.Reason)
	}
	if err != nil {
		return err
	}
	return p.Update(ctx, o)
}

func checkOutcome(out RailOutcome) error {
	switch {
	case out.Status != RailAccepted && out.Status != RailRejected:
		return fmt.Errorf("%w: unknown status %q", ErrInvalidNotification, out.Status)
	case out.Status == RailRejected && out.Reason == "":
		return fmt.Errorf("%w: a rejection without its reason", ErrInvalidNotification)
	case !validText(out.RailRef, maxRailRef) || !validText(out.Reason, maxReason):
		return fmt.Errorf("%w: the reference and the reason are text of up to %d characters", ErrInvalidNotification, maxReason)
	}
	return nil
}

// tookAlready is nil if o, no longer sent, took out's outcome, and ErrOutcomeConflict if it took another.
func tookAlready(o Order, out RailOutcome) error {
	if (out.Status == RailAccepted && o.Status == domain.StatusSettled) || (out.Status == RailRejected && o.Status == domain.StatusRejected) {
		return nil
	}
	return fmt.Errorf("%w: order %s is %s and the rail says %s", ErrOutcomeConflict, o.ID, o.Status, out.Status)
}

// settle moves o to settled and captures its hold with its charges.
func settle(ctx context.Context, p Settling, o *Order) error {
	if err := o.Settle(); err != nil {
		return err
	}
	accounts, err := p.Accounts(ctx, *o)
	if err != nil {
		return err
	}
	charges := make([]postingrules.Charge, 0, len(o.Fees)+1)
	for _, f := range o.Fees {
		charges = append(charges, postingrules.Charge{Amount: f.Amount, Counter: accounts.FeeIncome})
	}
	if o.GMF.Sign() > 0 {
		charges = append(charges, postingrules.Charge{Amount: o.GMF, Counter: accounts.GMFPayable})
	}
	entries, err := postingrules.Charges(accounts.Customer, charges)
	if err != nil {
		return err
	}
	return p.Capture(ctx, *o, entries)
}

// reject moves o to rejected for reason and releases its hold.
func reject(ctx context.Context, p Settling, o *Order, reason string) error {
	if err := o.Reject(reason); err != nil {
		return err
	}
	return p.Release(ctx, *o)
}
