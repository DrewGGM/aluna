//go:build integration

package ledger_test

import (
	"errors"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// afterTheHolds es un now en que ya vencieron los holds de una hora de estas pruebas.
func afterTheHolds() time.Time { return time.Now().Add(2 * time.Hour) }

func expireDue(t *testing.T, now time.Time, batch int) int {
	t.Helper()
	n, err := svc().ExpireDueHolds(t.Context(), postgres.NewUnitOfWork(testPool), now, batch)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func mustPlaceHold(t *testing.T, account, counter ids.ID, units int64, expiresIn time.Duration) ids.ID {
	t.Helper()
	hold, err := placeHold(t, account, counter, units, expiresIn)
	if err != nil {
		t.Fatal(err)
	}
	return hold.ID()
}

func statusOf(t *testing.T, hold ids.ID) ledger.HoldStatus {
	t.Helper()
	var status ledger.HoldStatus
	if err := testPool.QueryRow(t.Context(), `SELECT status FROM ledger.holds WHERE id = $1`, hold).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// TestExpireDueHoldsReleasesThePending: los holds vencidos, de un pasivo y de un activo, quedan expired
// y su importe sale del pendiente del lado en que se retuvo; uno que vence después sigue pending. Con
// lotes de uno, ExpireDueHolds sigue hasta vaciar los vencidos. La base es de todo el paquete: el
// total cuenta también los holds pending que dejaron otras pruebas.
func TestExpireDueHoldsReleasesThePending(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	cash, funding := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	mustPostRef(t, transfer(t, cash.ID, funding.ID, ids.New().String()))
	due := []ids.ID{
		mustPlaceHold(t, deposit.ID, clearing.ID, 30, time.Hour), mustPlaceHold(t, deposit.ID, clearing.ID, 20, time.Hour),
		mustPlaceHold(t, cash.ID, funding.ID, 60, time.Hour),
	}
	later := mustPlaceHold(t, deposit.ID, clearing.ID, 10, 3*time.Hour)

	if n := expireDue(t, afterTheHolds(), 1); n < len(due) {
		t.Fatalf("expiró %d holds, se esperaban al menos %d", n, len(due))
	}

	for _, id := range due {
		if s := statusOf(t, id); s != ledger.HoldExpired {
			t.Errorf("hold %s = %s, se esperaba expired", id, s)
		}
	}
	if s := statusOf(t, later); s != ledger.HoldPending {
		t.Errorf("el hold que vence después = %s, se esperaba pending", s)
	}
	if b := balanceOf(t, deposit.ID); b.PendingDebits != 10 || b.Posted != -100 {
		t.Errorf("depósito = %+v, se esperaban 10 pendientes (el hold no vencido) y el posted intacto", b)
	}
	if b := balanceOf(t, cash.ID); b.PendingCredits != 0 || b.Posted != 100 {
		t.Errorf("caja = %+v, se esperaba nada pendiente y el posted intacto", b)
	}
}

// TestABatchReleasesEveryHoldOfAnAccount: dos holds de la misma cuenta en un lote liberan entre los dos
// el pendiente completo: se suman, no se pisan.
func TestABatchReleasesEveryHoldOfAnAccount(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	mustPlaceHold(t, deposit.ID, clearing.ID, 30, time.Hour)
	mustPlaceHold(t, deposit.ID, clearing.ID, 20, time.Hour)

	expireDue(t, afterTheHolds(), 100)

	if b := balanceOf(t, deposit.ID); b.PendingDebits != 0 {
		t.Fatalf("depósito = %+v, se esperaba nada pendiente tras expirar sus dos holds", b)
	}
}

// TestAHoldBeingCapturedIsNotExpired: la expiración salta el hold que un capture tiene bloqueado, sin
// esperarlo; el capture confirma y el hold queda captured.
func TestAHoldBeingCapturedIsNotExpired(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	hold := mustPlaceHold(t, deposit.ID, clearing.ID, 50, time.Hour)
	_, commitCapture := captureUncommitted(t, hold)

	expireDue(t, afterTheHolds(), 10)

	if err := commitCapture(); err != nil {
		t.Fatalf("el capture debe confirmar: %v", err)
	}
	if s := statusOf(t, hold); s != ledger.HoldCaptured {
		t.Fatalf("hold = %s, se esperaba captured", s)
	}
	if b := balanceOf(t, deposit.ID); b.Posted != -50 || b.PendingDebits != 0 {
		t.Fatalf("depósito = %+v, se esperaba posted -50 y nada pendiente", b)
	}
}

// TestAnExpiredHoldIsNotCaptured: si la expiración bloquea el hold primero, el capture lo encuentra
// expired.
func TestAnExpiredHoldIsNotCaptured(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	hold := mustPlaceHold(t, deposit.ID, clearing.ID, 50, time.Hour)
	expireDue(t, afterTheHolds(), 10)

	if _, err := captureHold(t, hold, 50); !errors.Is(err, ledger.ErrHoldNotPending) {
		t.Fatalf("capturar un hold expirado = %v, se esperaba ErrHoldNotPending", err)
	}
	if b := balanceOf(t, deposit.ID); b.Posted != -100 || b.PendingDebits != 0 {
		t.Fatalf("depósito = %+v, se esperaba el posted intacto y nada pendiente", b)
	}
}

// TestAHoldIsNotDueAtItsExpiry: a la hora exacta de su vencimiento el hold todavía se puede capturar
// (Hold.Expire exige que ya haya pasado), así que la expiración no lo toma ni falla el lote.
func TestAHoldIsNotDueAtItsExpiry(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	hold, err := placeHold(t, deposit.ID, clearing.ID, 50, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	expireDue(t, hold.ExpiresAt(), 10)

	if s := statusOf(t, hold.ID()); s != ledger.HoldPending {
		t.Fatalf("hold a la hora de su vencimiento = %s, se esperaba pending", s)
	}
}

func TestExpireDueHoldsRejectsAnEmptyBatch(t *testing.T) {
	if _, err := svc().ExpireDueHolds(t.Context(), postgres.NewUnitOfWork(testPool), afterTheHolds(), 0); err == nil {
		t.Fatal("un lote de 0 debe fallar: no avanzaría nunca")
	}
}
