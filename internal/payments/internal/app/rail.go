// Package app holds the payments use cases and the ports they need: the rail that carries a payment to the
// beneficiary's bank (FR-31).
package app

import (
	"context"
	"errors"
	"time"

	"github.com/DrewGGM/aluna/internal/money"
)

// sendTimeout is the longest the core waits for a rail to take a message (FR-31).
const sendTimeout = 15 * time.Second

// ErrRailRefused means the rail did not take the message: unlike a rail that does not answer, the payment
// certainly did not leave.
var ErrRailRefused = errors.New("the rail did not take the message")

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
)

// RailOutcome is the rail's word on how a message ended: accepted, or rejected with its ISO 20022 reason code.
type RailOutcome struct {
	EndToEndID, TxID, RailRef string
	Status                    RailStatus
	Reason                    string
}

// RailPort is a payment rail (Bre-B, ACH): its adapter speaks its protocol, and the core speaks of messages
// (architecture §3, anticorruption layer). Send should return when ctx ends, though app.Send does not rely on
// it, and is ErrRailRefused, wrapped, if the rail did not take the message.
type RailPort interface {
	Send(ctx context.Context, msg OutboundMessage) (SendAck, error)
	Name() string
}

// Send hands msg to rail and waits up to sendTimeout for the rail to take it, even if the rail does not
// return when its context ends. After that the outcome is unknown: the rail may still have taken it and
// notify it later.
func Send(ctx context.Context, rail RailPort, msg OutboundMessage) (SendAck, error) {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	type answer struct {
		ack SendAck
		err error
	}
	answered := make(chan answer, 1) // a rail that answers late does not block
	go func() {
		ack, err := rail.Send(ctx, msg)
		answered <- answer{ack, err}
	}()
	select {
	case a := <-answered:
		return a.ack, a.err
	case <-ctx.Done():
		return SendAck{}, ctx.Err()
	}
}
