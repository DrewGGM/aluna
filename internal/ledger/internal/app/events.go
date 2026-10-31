package app

import (
	"context"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger/internal/domain"
	"github.com/DrewGGM/aluna/internal/money"
)

// Events publishes the ledger events within the use case's transaction (ADR-0007): if the transaction
// rolls back, the event does not go out either.
type Events interface {
	Emit(ctx context.Context, e Event) error
}

// Event is a ledger event (architecture §8).
type Event struct {
	Type, Subject string
	LegalEntityID ids.ID
	SchemaVersion int
	Data          Payload
}

// Payload is an event's content: transactionPayload or holdPayload. It carries identifiers, amounts,
// currencies, statuses and dates; never the actor or the metadata, which may bring personal data
// (Constitution VII).
type Payload interface{ ledgerPayload() }

// Ledger event types; schemaVersion is the version their .v1 suffix states.
const (
	typeTransactionPosted   = "aluna.ledger.transaction.posted.v1"
	typeTransactionReversed = "aluna.ledger.transaction.reversed.v1"
	typeHoldPlaced          = "aluna.ledger.hold.placed.v1"
	typeHoldCaptured        = "aluna.ledger.hold.captured.v1"
	typeHoldReleased        = "aluna.ledger.hold.released.v1"
	typeHoldExpired         = "aluna.ledger.hold.expired.v1"

	schemaVersion = 1
)

type amountPayload struct {
	Amount   int64  `json:"amount"` // minor units, like Money in the API
	Currency string `json:"currency"`
}

func amountOf(a money.Amount) amountPayload {
	return amountPayload{Amount: a.Units(), Currency: a.Currency().Code()}
}

type entryPayload struct {
	AccountID ids.ID        `json:"account_id"`
	Amount    amountPayload `json:"amount"` // debit positive, credit negative
}

type transactionPayload struct {
	TransactionID ids.ID         `json:"transaction_id"`
	ReversesID    ids.ID         `json:"reverses_id,omitzero"`
	Code          string         `json:"code"`
	Source        string         `json:"source"`
	ExternalRef   string         `json:"external_ref"`
	ValueDate     string         `json:"value_date"`
	BusinessDate  string         `json:"business_date"`
	PostedAt      time.Time      `json:"posted_at"`
	Entries       []entryPayload `json:"entries"`
}

func (transactionPayload) ledgerPayload() {}

// transactionEvent is the typ event of transaction t.
func transactionEvent(typ string, t Transaction) Event {
	entries := make([]entryPayload, len(t.Entries))
	for i, e := range t.Entries {
		entries[i] = entryPayload{AccountID: e.AccountID, Amount: amountOf(e.Amount)}
	}
	return Event{
		Type: typ, Subject: "transactions/" + t.ID.String(), LegalEntityID: t.LegalEntityID, SchemaVersion: schemaVersion,
		Data: transactionPayload{
			TransactionID: t.ID, ReversesID: t.ReversesID, Code: t.Code, Source: t.Source, ExternalRef: t.ExternalRef,
			ValueDate: t.ValueDate.Format(time.DateOnly), BusinessDate: t.BusinessDate.Format(time.DateOnly),
			PostedAt: t.PostedAt.UTC(), Entries: entries,
		},
	}
}

type holdPayload struct {
	HoldID           ids.ID            `json:"hold_id"`
	AccountID        ids.ID            `json:"account_id"`
	CounterAccountID ids.ID            `json:"counter_account_id"`
	Amount           amountPayload     `json:"amount"`
	Captured         amountPayload     `json:"captured"`
	Status           domain.HoldStatus `json:"status"`
	ExpiresAt        time.Time         `json:"expires_at"`
	TransactionID    ids.ID            `json:"transaction_id,omitzero"` // the capture's
}

func (holdPayload) ledgerPayload() {}

// holdEvent is the typ event of the entity's hold h; captureTx is a capture's transaction or zero.
func holdEvent(typ string, entity ids.ID, h Hold, captureTx ids.ID) Event {
	return Event{
		Type: typ, Subject: "holds/" + h.ID().String(), LegalEntityID: entity, SchemaVersion: schemaVersion,
		Data: holdPayload{
			HoldID: h.ID(), AccountID: h.AccountID(), CounterAccountID: h.CounterAccountID(), Amount: amountOf(h.Amount()),
			Captured: amountOf(h.Captured()), Status: h.Status(), ExpiresAt: h.ExpiresAt().UTC(), TransactionID: captureTx,
		},
	}
}
