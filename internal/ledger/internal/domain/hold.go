package domain

import (
	"errors"
	"fmt"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
)

var (
	// ErrInvalidHold indica un hold o un capture con datos inválidos: importe no positivo, sin moneda,
	// sin identificador, sin cuenta, sin instante de creación, que vence al crearse o, al
	// reconstruirlo, una fila que no cumple las invariantes del hold.
	ErrInvalidHold = errors.New("hold inválido")
	// ErrHoldNotPending indica una operación sobre un hold que ya terminó.
	ErrHoldNotPending = errors.New("el hold no está pendiente")
	// ErrAmountExceedsHold indica un capture por más de lo retenido.
	ErrAmountExceedsHold = errors.New("el capture supera el hold")
	// ErrHoldExpired indica un capture de un hold pending después de su vencimiento.
	ErrHoldExpired = errors.New("el hold venció")
	// ErrHoldNotDue indica que se quiso expirar un hold antes de su vencimiento.
	ErrHoldNotDue = errors.New("el hold todavía no vence")
)

// HoldStatus es el estado de un hold (FR-6): nace pending y termina en exactamente uno de los otros.
type HoldStatus string

// Estados de un hold.
const (
	HoldPending  HoldStatus = "pending"
	HoldCaptured HoldStatus = "captured"
	HoldReleased HoldStatus = "released"
	HoldExpired  HoldStatus = "expired"
)

// Hold es una reserva de fondos de dos fases sobre una cuenta: mientras está pending reduce el
// disponible; al capturarlo se asienta (total o parcialmente, y el resto se libera), al liberarlo o al
// expirar el disponible vuelve. Es inmutable: cada transición devuelve un hold nuevo.
type Hold struct {
	id        ids.ID
	accountID ids.ID
	amount    money.Amount
	captured  money.Amount
	status    HoldStatus
	expiresAt time.Time
}

// NewHoldInput son los datos de un hold nuevo. Now es el instante de creación, que da el reloj del
// caso de uso: el dominio no lee la hora.
type NewHoldInput struct {
	ID, AccountID ids.ID
	Amount        money.Amount
	ExpiresAt     time.Time
	Now           time.Time
}

// NewHold crea un hold pending. El importe es positivo y vence después de Now.
func NewHold(in NewHoldInput) (Hold, error) {
	if err := checkIdentity(in.ID, in.AccountID, in.Amount); err != nil {
		return Hold{}, err
	}
	if in.Now.IsZero() || !in.ExpiresAt.After(in.Now) {
		return Hold{}, fmt.Errorf("%w: vence %v, no después de su creación %v", ErrInvalidHold, in.ExpiresAt, in.Now)
	}
	return Hold{
		id: in.ID, accountID: in.AccountID, amount: in.Amount, captured: money.New(0, in.Amount.Currency()),
		status: HoldPending, expiresAt: in.ExpiresAt,
	}, nil
}

// HoldSnapshot es un hold tal como se guarda: lo que se lee de la base para reconstruirlo.
type HoldSnapshot struct {
	ID, AccountID    ids.ID
	Amount, Captured money.Amount
	Status           HoldStatus
	ExpiresAt        time.Time
}

// RestoreHold reconstruye un hold guardado, en cualquier estado, también pending ya vencido (para que
// Capture dé ErrHoldExpired y Expire lo cierre). Rechaza una fila que no cumple las invariantes:
// estado conocido, lo capturado en la moneda del hold y entre cero y el importe, y distinto de cero
// solo si está captured.
func RestoreHold(s HoldSnapshot) (Hold, error) {
	if err := checkIdentity(s.ID, s.AccountID, s.Amount); err != nil {
		return Hold{}, err
	}
	if err := checkCaptured(s); err != nil {
		return Hold{}, err
	}
	if s.ExpiresAt.IsZero() {
		return Hold{}, fmt.Errorf("%w: sin vencimiento", ErrInvalidHold)
	}
	return Hold{
		id: s.ID, accountID: s.AccountID, amount: s.Amount, captured: s.Captured, status: s.Status, expiresAt: s.ExpiresAt,
	}, nil
}

// checkCaptured valida el estado guardado y lo capturado contra él.
func checkCaptured(s HoldSnapshot) error {
	switch s.Status {
	case HoldPending, HoldCaptured, HoldReleased, HoldExpired:
	default:
		return fmt.Errorf("%w: estado %q", ErrInvalidHold, s.Status)
	}
	if s.Captured.Currency() != s.Amount.Currency() || s.Captured.Sign() < 0 || s.Captured.Units() > s.Amount.Units() {
		return fmt.Errorf("%w: capturado %d %q de %d %q", ErrInvalidHold, s.Captured.Units(), s.Captured.Currency().Code(),
			s.Amount.Units(), s.Amount.Currency().Code())
	}
	if (s.Captured.Sign() > 0) != (s.Status == HoldCaptured) {
		return fmt.Errorf("%w: %s con %d capturado", ErrInvalidHold, s.Status, s.Captured.Units())
	}
	return nil
}

// checkIdentity valida lo que todo hold tiene: identificador, cuenta e importe positivo con moneda.
func checkIdentity(id, accountID ids.ID, amount money.Amount) error {
	switch {
	case id == (ids.ID{}) || accountID == (ids.ID{}):
		return fmt.Errorf("%w: sin identificador o sin cuenta", ErrInvalidHold)
	case amount.Sign() <= 0 || amount.Currency() == (money.Currency{}):
		return fmt.Errorf("%w: importe %d %q", ErrInvalidHold, amount.Units(), amount.Currency().Code())
	}
	return nil
}

// ID es el identificador del hold.
func (h Hold) ID() ids.ID { return h.id }

// AccountID es la cuenta cuyos fondos retiene el hold.
func (h Hold) AccountID() ids.ID { return h.accountID }

// Amount es lo retenido, positivo.
func (h Hold) Amount() money.Amount { return h.amount }

// Captured es lo asentado al capturarlo; cero si no se capturó.
func (h Hold) Captured() money.Amount { return h.captured }

// Status es el estado del hold.
func (h Hold) Status() HoldStatus { return h.status }

// ExpiresAt es el vencimiento: se puede capturar hasta ese instante inclusive, y el job lo expira
// cuando expires_at < now.
func (h Hold) ExpiresAt() time.Time { return h.expiresAt }

// CaptureResult es lo que deja un capture: Captured se asienta y Released vuelve al disponible.
type CaptureResult struct {
	Captured, Released money.Amount
}

// Capture asienta units del hold en now, hasta su vencimiento inclusive. Un capture parcial libera el
// resto (FR-6). El estado se revisa antes que el vencimiento: un hold que ya expiró responde
// ErrHoldNotPending, no ErrHoldExpired.
func (h Hold) Capture(units int64, now time.Time) (Hold, CaptureResult, error) {
	if err := h.requirePending(); err != nil {
		return Hold{}, CaptureResult{}, err
	}
	switch {
	case now.After(h.expiresAt):
		return Hold{}, CaptureResult{}, fmt.Errorf("%w: venció %v", ErrHoldExpired, h.expiresAt)
	case units <= 0:
		return Hold{}, CaptureResult{}, fmt.Errorf("%w: capture de %d", ErrInvalidHold, units)
	case units > h.amount.Units():
		return Hold{}, CaptureResult{}, fmt.Errorf("%w: %d de %d", ErrAmountExceedsHold, units, h.amount.Units())
	}
	cur := h.amount.Currency()
	captured := money.New(units, cur)
	next := h
	next.status, next.captured = HoldCaptured, captured
	return next, CaptureResult{Captured: captured, Released: money.New(h.amount.Units()-units, cur)}, nil
}

// Release libera el hold completo: el disponible vuelve.
func (h Hold) Release() (Hold, error) {
	if err := h.requirePending(); err != nil {
		return Hold{}, err
	}
	next := h
	next.status = HoldReleased
	return next, nil
}

// Expire cierra el hold vencido en now (expires_at < now): el disponible vuelve.
func (h Hold) Expire(now time.Time) (Hold, error) {
	if err := h.requirePending(); err != nil {
		return Hold{}, err
	}
	if !now.After(h.expiresAt) {
		return Hold{}, fmt.Errorf("%w: vence %v", ErrHoldNotDue, h.expiresAt)
	}
	next := h
	next.status = HoldExpired
	return next, nil
}

func (h Hold) requirePending() error {
	if h.status != HoldPending {
		return fmt.Errorf("%w: está %s", ErrHoldNotPending, h.status)
	}
	return nil
}
