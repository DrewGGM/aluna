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

// ErrHoldNotFound indica un hold que no existe o que es de otra entidad legal.
var ErrHoldNotFound = errors.New("hold no encontrado")

// Cabecera de la transacción que asienta un capture. La referencia externa es el hold: un hold se
// asienta a lo sumo una vez.
const (
	holdSource             = "ledger.hold"
	holdCaptureCode        = "hold_capture"
	holdCaptureRuleVersion = "hold-capture.v1"
)

// Hold es un hold con su contrapartida, la cuenta que recibe lo capturado. Solo se lee: sus
// transiciones las hacen los casos de uso, que las guardan.
type Hold struct {
	hold             domain.Hold
	counterAccountID ids.ID
}

// RestoredHold arma un Hold leído de la base.
func RestoredHold(h domain.Hold, counterAccountID ids.ID) Hold {
	return Hold{hold: h, counterAccountID: counterAccountID}
}

// ID es el identificador del hold.
func (h Hold) ID() ids.ID { return h.hold.ID() }

// AccountID es la cuenta cuyos fondos retiene.
func (h Hold) AccountID() ids.ID { return h.hold.AccountID() }

// CounterAccountID es la cuenta que recibe lo capturado.
func (h Hold) CounterAccountID() ids.ID { return h.counterAccountID }

// Amount es lo retenido.
func (h Hold) Amount() money.Amount { return h.hold.Amount() }

// Captured es lo asentado al capturarlo; cero si no se capturó.
func (h Hold) Captured() money.Amount { return h.hold.Captured() }

// Status es el estado del hold.
func (h Hold) Status() domain.HoldStatus { return h.hold.Status() }

// ExpiresAt es el vencimiento.
func (h Hold) ExpiresAt() time.Time { return h.hold.ExpiresAt() }

func (h Hold) with(next domain.Hold) Hold {
	h.hold = next
	return h
}

// PlaceHoldInput pide retener Amount de AccountID hasta ExpiresAt; lo capturado irá a
// CounterAccountID.
type PlaceHoldInput struct {
	LegalEntityID, AccountID, CounterAccountID ids.ID
	Amount                                     money.Amount
	ExpiresAt                                  time.Time
}

// CaptureInput pide asentar Amount, en la moneda del hold, con la fecha de negocio BusinessDate.
type CaptureInput struct {
	LegalEntityID, HoldID ids.ID
	Amount                money.Amount
	BusinessDate          time.Time
	Actor, Channel        string
}

// ReleaseInput pide liberar un hold.
type ReleaseInput struct {
	LegalEntityID, HoldID ids.ID
}

// HoldRecords es la persistencia de las filas de holds.
type HoldRecords interface {
	// DatabaseNow es la hora de la base, la misma que fija posted_at.
	DatabaseNow(ctx context.Context) (time.Time, error)
	InsertHold(ctx context.Context, entity ids.ID, h Hold) error
	// LockHold bloquea (FOR UPDATE) y devuelve el hold de la entidad, o ErrHoldNotFound.
	LockHold(ctx context.Context, entity, id ids.ID) (Hold, error)
	// CloseHold guarda el resultado de un hold que salió de pending; captureTxID es cero si no se capturó.
	CloseHold(ctx context.Context, h Hold, captureTxID ids.ID) error
}

// HoldStore es la persistencia de los casos de uso de holds: un capture asienta con post.
type HoldStore interface {
	PostingStore
	HoldRecords
}

// PlaceHold retiene fondos de la cuenta origen (FR-6): valida el disponible como si el importe saliera
// ya, crea el hold pending y lo suma al pendiente del lado por el que la cuenta pierde saldo
// (pending_debits en un pasivo, pending_credits en un activo). El vencimiento se guarda en UTC y con
// microsegundos, la precisión de la base, para que el hold creado sea igual al que se lee. Emite
// hold.placed.
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

// CaptureHold asienta parte o todo un hold (FR-6). Bloquea el hold antes que los saldos (ADR-0004); la
// hora del capture es la de la base tras el lock, así que entre un capture y la expiración gana el
// primero que bloquea el hold. Lo capturado se asienta del origen a la contrapartida y el pendiente
// completo se libera en la misma escritura del posting, después de todas sus validaciones: un capture
// rechazado no deja nada escrito. Emite hold.captured.
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
		return Hold{}, Transaction{}, fmt.Errorf("%w: capture en %s, hold en %s", ErrCurrencyMismatch, in.Amount.Currency().Code(), h.Amount().Currency().Code())
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
	// su evento es hold.captured, con la transacción: el posting del capture no emite transaction.posted
	if err := events.Emit(ctx, holdEvent(typeHoldCaptured, in.LegalEntityID, captured, posted.ID)); err != nil {
		return Hold{}, Transaction{}, err
	}
	return captured, posted, nil
}

// captureInput es el posting de un capture: outgoing sale del origen y entra a la contrapartida.
func captureInput(in CaptureInput, h Hold, outgoing money.Amount) PostInput {
	incoming := money.New(-outgoing.Units(), outgoing.Currency()) // |outgoing| ≤ el importe del hold: no desborda
	return PostInput{
		LegalEntityID: in.LegalEntityID, Code: holdCaptureCode, ValueDate: in.BusinessDate, BusinessDate: in.BusinessDate,
		Source: holdSource, ExternalRef: h.ID().String(), Actor: in.Actor, Channel: in.Channel, RuleVersion: holdCaptureRuleVersion,
		Entries: []domain.EntryInput{{AccountID: h.AccountID(), Amount: outgoing}, {AccountID: h.CounterAccountID(), Amount: incoming}},
	}
}

// ReleaseHold libera un hold pending: su importe sale del pendiente y el disponible vuelve. Emite
// hold.released.
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

// holdOrigin carga origen y contrapartida de un hold nuevo, que deben ser distintas, de la entidad y en
// la moneda del hold; el origen además sync, porque solo una cuenta sync lleva su pendiente. Devuelve
// el origen. Un hold ya guardado no se revalida: lo garantizan sus FK y holds_distinct_accounts.
func holdOrigin(ctx context.Context, store PostingStore, in PlaceHoldInput) (domain.Account, error) {
	if in.AccountID == in.CounterAccountID {
		return domain.Account{}, fmt.Errorf("%w: origen y contrapartida son la misma cuenta", domain.ErrInvalidHold)
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
			return domain.Account{}, fmt.Errorf("%w: cuenta %s en %s, hold en %s", ErrCurrencyMismatch, a.Code, a.Currency.Code(), in.Amount.Currency().Code())
		}
	}
	origin := accounts[in.AccountID]
	if origin.Mode != domain.BalanceModeSync {
		return domain.Account{}, fmt.Errorf("%w: la cuenta %s es async y no lleva pendiente", domain.ErrInvalidHold, origin.Code)
	}
	return origin, nil
}

// outgoingOf es amount con el signo con que sale de a: crédito en una cuenta de naturaleza débito,
// débito en una de naturaleza crédito.
func outgoingOf(a domain.Account, amount money.Amount) money.Amount {
	return money.New(-domain.NaturalBalance(a.Kind, amount.Units()), amount.Currency())
}

// pendingOf reparte outgoing entre los dos pendientes, que se guardan positivos: un débito va a
// pending_debits y un crédito a pending_credits.
func pendingOf(outgoing money.Amount) (debits, credits int64) {
	if outgoing.Sign() > 0 {
		return outgoing.Units(), 0
	}
	return 0, -outgoing.Units()
}
