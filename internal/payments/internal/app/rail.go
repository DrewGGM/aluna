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
)

// RailOutcome is the rail's word on how a message ended: accepted, or rejected with its ISO 20022 reason code.
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

// RailPort is a payment rail (Bre-B, ACH): its adapter speaks its protocol, and the core speaks of messages
// (architecture §3, anticorruption layer). Send should return when ctx ends, though app.Send does not rely on
// it, and is ErrRailRefused, wrapped, if the rail did not take the message. Outcome is the outcome n carries,
// and Transfer the transfer it carries, if the rail sent it, and ErrUnverifiedNotification, wrapped, otherwise.
type RailPort interface {
	Send(ctx context.Context, msg OutboundMessage) (SendAck, error)
	Outcome(n Notification) (RailOutcome, error)
	Transfer(n Notification) (InboundTransfer, error)
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
