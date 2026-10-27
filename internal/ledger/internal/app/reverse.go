package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger/internal/domain"
)

// ErrAlreadyReversed indica una transacción que ya tiene su reversa: Reverse devuelve la reversa
// existente junto con este error.
var ErrAlreadyReversed = errors.New("la transacción ya se revirtió")

// Cabecera de las reversas.
const (
	reversalSource  = "ledger.reversal"
	reversalCode    = "reversal"
	reversalChannel = "ledger"
)

// ReverseInput pide revertir la transacción TransactionID de la entidad LegalEntityID.
type ReverseInput struct {
	LegalEntityID ids.ID
	TransactionID ids.ID
	Reason, Actor string
	BusinessDate  time.Time
}

// ReversalStore es la persistencia de Reverse: la de Post y la lectura de la transacción revertida.
type ReversalStore interface {
	PostingStore
	TransactionOf(ctx context.Context, entity, id ids.ID) (Transaction, error)
}

// Reverse asienta la reversa de una transacción (FR-3): sus entries con el signo contrario, con la
// fecha valor de la original y la fecha de negocio de in. Pasa por Post, así que respeta los límites:
// revertir un depósito ya gastado es ErrInsufficientFunds. Si la transacción ya tiene reversa, la
// devuelve con ErrAlreadyReversed y no escribe nada.
func Reverse(ctx context.Context, store ReversalStore, in ReverseInput) (Transaction, error) {
	if in.Reason == "" {
		return Transaction{}, fmt.Errorf("%w: falta el motivo de la reversa", ErrInvalidPosting)
	}
	original, err := store.TransactionOf(ctx, in.LegalEntityID, in.TransactionID)
	if err != nil {
		return Transaction{}, err
	}
	entries, err := domain.ReversalEntries(original.Entries)
	if err != nil {
		return Transaction{}, err
	}
	// La referencia externa es la transacción revertida: la unicidad de (source, external_ref) deja a lo
	// sumo una reversa por transacción, también entre dos reversas concurrentes.
	reversal, err := post(ctx, store, PostInput{
		LegalEntityID: in.LegalEntityID, Code: reversalCode, ValueDate: original.ValueDate, BusinessDate: in.BusinessDate,
		Source: reversalSource, ExternalRef: original.ID.String(), Actor: in.Actor, Channel: reversalChannel,
		RuleVersion: domain.ReversalRuleVersion, ReversesID: original.ID, Metadata: map[string]string{"reason": in.Reason},
		Entries: entries,
	}, pendingRelease{})
	if !errors.Is(err, ErrDuplicateExternalRef) {
		return reversal, err
	}
	if reversal.ReversesID != original.ID { // la base no impide que SQL directo ocupe la referencia: se falla cerrado
		return Transaction{}, fmt.Errorf("la referencia de la reversa de %s la tiene %s, que no la revierte", original.ID, reversal.ID)
	}
	return reversal, fmt.Errorf("%w: %s ya la revierte %s", ErrAlreadyReversed, original.ID, reversal.ID)
}
