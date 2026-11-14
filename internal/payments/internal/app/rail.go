// Package app holds the payments use cases and the ports they need: the rail that carries a payment to the
// beneficiary's bank (FR-31).
package app

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
)

// sendTimeout is the longest the core waits for a rail to take a message (FR-31).
const sendTimeout = 15 * time.Second

var (
	// ErrRailRefused means the rail did not take the message: unlike a rail that does not answer, the payment
	// certainly did not leave.
	ErrRailRefused = errors.New("the rail did not take the message")
	// ErrUnverifiedNotification means a notification the rail did not sign, or signed too long ago: it may be
	// forged or replayed (TH-7).
	ErrUnverifiedNotification = errors.New("the notification is not the rail's")
	// ErrInvalidNotification means a notification of the rail whose outcome the core cannot read.
	ErrInvalidNotification = errors.New("invalid rail notification")
)

// AccountRef is a party of a payment at its bank, by account number or by key (a Bre-B "llave"): one of the
// two.
type AccountRef struct {
	Number, Key string
}

// OutboundMessage is what the core hands a rail of a credit transfer: the minimum of an ISO 20022 pacs.008,
// its identifiers, its interbank amount, and its debtor and creditor.
type OutboundMessage struct {
	EndToEndID, InstrID, TxID string
	Amount                    money.Amount
	Debtor, Creditor          AccountRef
}

// SendAck is the rail taking a message, with its own reference to it. It is not the outcome: that comes later,
// as the rail's notification (RailOutcome).
type SendAck struct {
	RailRef string
}

// RailStatus is how a message ended at the rail.
type RailStatus string

// Rail statuses.
const (
	RailAccepted RailStatus = "accepted"
	RailRejected RailStatus = "rejected"
	RailReturned RailStatus = "returned" // accepted, settled, and later sent back by the beneficiary's bank
)

// RailOutcome is the rail's word on how a message ended: accepted, or rejected with its ISO 20022 reason code; or,
// after it settled, returned by the beneficiary's bank with its ISO 20022 return reason.
type RailOutcome struct {
	EndToEndID, TxID, RailRef string
	Status                    RailStatus
	Reason                    string
}

// InboundTransfer is a credit transfer another bank sent through the rail to a customer of the entity, a
// pacs.008 received: its ISO 20022 identifiers, the rail's reference, DebtorAgent, the bank that sent it,
// LegalEntityID, the entity it is for (its creditor agent), its interbank amount, its interbank settlement
// date, and its creditor by account number or by key.
type InboundTransfer struct {
	LegalEntityID                      ids.ID
	EndToEndID, InstrID, TxID, RailRef string
	DebtorAgent                        string
	Amount                             money.Amount
	SettlementDate                     time.Time
	Creditor                           AccountRef
}

// Notification is a rail's notification as it reached the core: its headers and its body.
type Notification struct {
	Header http.Header
	Body   []byte
}

// ReturnMessage is what the core hands a rail to send a transfer it received back to the bank that sent it, the
// minimum of an ISO 20022 pacs.004: the transfer's end-to-end id, that bank (its debtor agent), its amount and why.
type ReturnMessage struct {
	EndToEndID, DebtorAgent string
	Amount                  money.Amount
	Reason                  string
}

// RailPort is a payment rail (Bre-B, ACH): its adapter speaks its protocol, and the core speaks of messages
// (architecture §3, anticorruption layer). Send should return when ctx ends, though app.Send does not rely on
// it, and is ErrRailRefused, wrapped, if the rail did not take the message; Return is the same for a return.
// Outcome is the outcome n carries, and Transfer the transfer it carries, if the rail sent it, and
// ErrUnverifiedNotification, wrapped, otherwise.
type RailPort interface {
	Send(ctx context.Context, msg OutboundMessage) (SendAck, error)
	Return(ctx context.Context, msg ReturnMessage) error
	Outcome(n Notification) (RailOutcome, error)
	Transfer(n Notification) (InboundTransfer, error)
	Name() string
}

// Send hands msg to rail and waits up to sendTimeout for the rail to take it, even if the rail does not
// return when its context ends. After that the outcome is unknown: the rail may still have taken it and
// notify it later.
func Send(ctx context.Context, rail RailPort, msg OutboundMessage) (SendAck, error) {
	return withinRailTimeout(ctx, func(ctx context.Context) (SendAck, error) { return rail.Send(ctx, msg) })
}

// HandReturn hands msg to rail and waits for it as Send does.
func HandReturn(ctx context.Context, rail RailPort, msg ReturnMessage) error {
	_, err := withinRailTimeout(ctx, func(ctx context.Context) (struct{}, error) { return struct{}{}, rail.Return(ctx, msg) })
	return err
}

// withinRailTimeout runs call and waits up to sendTimeout for its answer, even if it does not return when its
// context ends.
func withinRailTimeout[T any](ctx context.Context, call func(context.Context) (T, error)) (T, error) { //nolint:ireturn // T is the rail's concrete answer (SendAck, or nothing for a return)
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	type answer struct {
		value T
		err   error
	}
	answered := make(chan answer, 1) // a rail that answers late does not block
	go func() {
		value, err := call(ctx)
		answered <- answer{value, err}
	}()
	select {
	case a := <-answered:
		return a.value, a.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}
