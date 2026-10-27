package app

import (
	"context"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger/internal/domain"
	"github.com/DrewGGM/aluna/internal/money"
)

// Events publica los eventos del ledger en la transacción del caso de uso (ADR-0007): si la transacción
// se revierte, el evento tampoco sale.
type Events interface {
	Emit(ctx context.Context, e Event) error
}

// Event es un evento del ledger (architecture §8).
type Event struct {
	Type, Subject string
	LegalEntityID ids.ID
	SchemaVersion int
	Data          Payload
}

// Payload es el contenido de un evento: transactionPayload o holdPayload. Lleva identificadores, montos,
// monedas, estados y fechas; nunca el actor ni la metadata, que pueden traer datos personales
// (Constitución VII).
type Payload interface{ ledgerPayload() }

// Tipos de los eventos del ledger; schemaVersion es la versión que dice su sufijo .v1.
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
	Amount   int64  `json:"amount"` // unidades menores, como Money en la API
	Currency string `json:"currency"`
}

func amountOf(a money.Amount) amountPayload {
	return amountPayload{Amount: a.Units(), Currency: a.Currency().Code()}
}

type entryPayload struct {
	AccountID ids.ID        `json:"account_id"`
	Amount    amountPayload `json:"amount"` // débito positivo, crédito negativo
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

// transactionEvent es el evento typ de la transacción t.
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
	TransactionID    ids.ID            `json:"transaction_id,omitzero"` // la del capture
}

func (holdPayload) ledgerPayload() {}

// holdEvent es el evento typ del hold h de la entidad; captureTx es la transacción de un capture o cero.
func holdEvent(typ string, entity ids.ID, h Hold, captureTx ids.ID) Event {
	return Event{
		Type: typ, Subject: "holds/" + h.ID().String(), LegalEntityID: entity, SchemaVersion: schemaVersion,
		Data: holdPayload{
			HoldID: h.ID(), AccountID: h.AccountID(), CounterAccountID: h.CounterAccountID(), Amount: amountOf(h.Amount()),
			Captured: amountOf(h.Captured()), Status: h.Status(), ExpiresAt: h.ExpiresAt().UTC(), TransactionID: captureTx,
		},
	}
}
