//go:build integration

package ledger_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// registeredEntity registra en el calendario una entidad con fecha de negocio 2025-07-07, junio de 2025
// cerrado y mayo en cierre, y le crea una caja y un depósito. Las entidades y los periodos los escribe
// quien opera el despliegue: la prueba vuelve al dueño de las tablas.
func registeredEntity(t *testing.T) (id ids.ID, cash, deposit ledger.Account) {
	t.Helper()
	id = ids.New()
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `INSERT INTO calendar.legal_entities (id, name, business_date, functional_currency)
			VALUES ($1, 'Entidad', '2025-07-07', 'COP')`, id); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), `INSERT INTO calendar.periods (legal_entity_id, period, status, closed_at, closed_by) VALUES
			($1, '2025-06', 'closed', clock_timestamp(), 'contador'), ($1, '2025-05', 'closing', NULL, NULL)`, id)
		return err
	})
	return id, accountOf(t, id, ledger.KindAsset), accountOf(t, id, ledger.KindLiability)
}

// depositOn es un depósito de 100 a la caja de la entidad con la fecha de negocio businessDate (cero: sin
// fecha).
func depositOn(t *testing.T, entity ids.ID, cash, deposit ledger.Account, businessDate time.Time) ledger.PostInput {
	t.Helper()
	in := transfer(t, cash.ID, deposit.ID, ids.New().String())
	in.LegalEntityID, in.BusinessDate = entity, businessDate
	return in
}

// in2025 es el día d del mes m de 2025, el año del calendario de registeredEntity.
func in2025(m time.Month, d int) time.Time { return time.Date(2025, m, d, 0, 0, 0, 0, time.UTC) }

// TestAPostingInAClosedPeriodIsRejected (FR-15): con fecha de negocio en junio, cerrado, el posting es
// ErrPeriodClosed y no deja nada; en mayo (en cierre), en julio (abierto) y en agosto (sin fila) pasa.
func TestAPostingInAClosedPeriodIsRejected(t *testing.T) {
	entity, cash, deposit := registeredEntity(t)

	_, _, err := postRef(t, depositOn(t, entity, cash, deposit, in2025(time.June, 30)))

	if !errors.Is(err, ledger.ErrPeriodClosed) {
		t.Fatalf("posting en junio = %v, se esperaba ErrPeriodClosed", err)
	}
	if b := balanceOf(t, cash.ID); b.Posted != 0 {
		t.Fatalf("saldo = %+v, el posting rechazado no debía asentar", b)
	}
	for _, d := range []time.Time{in2025(time.May, 30), in2025(time.July, 7), in2025(time.August, 1)} {
		if _, _, err := postRef(t, depositOn(t, entity, cash, deposit, d)); err != nil {
			t.Errorf("posting el %s = %v, el periodo no está cerrado", d.Format(time.DateOnly), err)
		}
	}
}

// TestAPostingWithoutBusinessDateTakesTheEntitys: sin fecha de negocio, el posting usa la de la entidad.
func TestAPostingWithoutBusinessDateTakesTheEntitys(t *testing.T) {
	entity, cash, deposit := registeredEntity(t)

	posted, _ := mustPostRef(t, depositOn(t, entity, cash, deposit, time.Time{}))

	if !posted.BusinessDate.Equal(in2025(time.July, 7)) {
		t.Fatalf("fecha de negocio = %v, se esperaba la de la entidad (2025-07-07)", posted.BusinessDate)
	}
}

// TestWithoutBusinessDateAnUnregisteredEntityFails: la entidad de paquete no está en el calendario, así
// que sin fecha de negocio no hay de dónde tomarla.
func TestWithoutBusinessDateAnUnregisteredEntityFails(t *testing.T) {
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)

	_, _, err := postRef(t, depositOn(t, entity, cash, deposit, time.Time{}))

	if !errors.Is(err, ledger.ErrLegalEntityNotFound) {
		t.Fatalf("posting sin fecha de una entidad sin calendario = %v, se esperaba ErrLegalEntityNotFound", err)
	}
}

// TestReversalsAndCapturesInAClosedPeriodAreRejected: la guarda vale para todo lo que asienta el ledger.
func TestReversalsAndCapturesInAClosedPeriodAreRejected(t *testing.T) {
	entity, cash, deposit := registeredEntity(t)
	original, _ := mustPostRef(t, depositOn(t, entity, cash, deposit, in2025(time.July, 7)))
	june := in2025(time.June, 30)

	_, err := tryReverse(t, ledger.ReverseInput{LegalEntityID: entity, TransactionID: original.ID, Reason: reason, Actor: "operador", BusinessDate: june})
	if !errors.Is(err, ledger.ErrPeriodClosed) {
		t.Errorf("reversa en junio = %v, se esperaba ErrPeriodClosed", err)
	}
	if b := balanceOf(t, cash.ID); b.Posted != 100 {
		t.Errorf("caja = %+v, la reversa rechazada no debía asentar", b)
	}

	hold, err := inTx(t, func(tx postgres.Tx) (ledger.Hold, error) {
		return svc.PlaceHold(t.Context(), tx, ledger.PlaceHoldInput{
			LegalEntityID: entity, AccountID: deposit.ID, CounterAccountID: cash.ID, Amount: money.New(50, cop(t)), ExpiresAt: time.Now().Add(time.Hour),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = inTx(t, func(tx postgres.Tx) (ledger.Transaction, error) {
		_, posted, err := svc.CaptureHold(t.Context(), tx, ledger.CaptureInput{
			LegalEntityID: entity, HoldID: hold.ID(), Amount: money.New(50, cop(t)), BusinessDate: june, Actor: "tester", Channel: "test",
		})
		return posted, err
	})
	if !errors.Is(err, ledger.ErrPeriodClosed) {
		t.Errorf("capture en junio = %v, se esperaba ErrPeriodClosed", err)
	}
	if b := balanceOf(t, deposit.ID); b.PendingDebits != 50 || b.Posted != -100 {
		t.Errorf("depósito = %+v, el capture rechazado no debía asentar ni liberar el hold", b)
	}
}

// closePeriod cierra el periodo yyyy-mm de la entidad, como lo hará el cierre (T-069).
func closePeriod(t *testing.T, entity ids.ID, period string) {
	t.Helper()
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO calendar.periods (legal_entity_id, period, status, closed_at, closed_by)
			VALUES ($1, $2, 'closed', clock_timestamp(), 'contador')`, entity, period)
		return err
	})
}

// TestARetryAfterTheCloseReturnsTheOriginal (FR-11): lo asentado en abril sigue siendo idempotente
// después de cerrar abril: el reintento del posting devuelve la original y el de la reversa, la reversa.
func TestARetryAfterTheCloseReturnsTheOriginal(t *testing.T) {
	entity, cash, deposit := registeredEntity(t)
	april := in2025(time.April, 30)
	in := depositOn(t, entity, cash, deposit, april)
	original, _ := mustPostRef(t, in)
	reverseApril := ledger.ReverseInput{LegalEntityID: entity, TransactionID: original.ID, Reason: reason, Actor: "operador", BusinessDate: april}
	reversal, err := tryReverse(t, reverseApril)
	if err != nil {
		t.Fatal(err)
	}
	closePeriod(t, entity, "2025-04")

	if again, duplicate := mustPostRef(t, in); !duplicate || again.ID != original.ID {
		t.Errorf("reintento del posting = %s (repetido %v), se esperaba la original %s", again.ID, duplicate, original.ID)
	}
	if again, err := tryReverse(t, reverseApril); !errors.Is(err, ledger.ErrAlreadyReversed) || again.ID != reversal.ID {
		t.Errorf("reintento de la reversa = %s, %v; se esperaba la reversa %s con ErrAlreadyReversed", again.ID, err, reversal.ID)
	}
}

// TestPeriodStatusOfTheCalendar: la fachada del calendario da el estado de cada mes (sin fila, abierto).
// Vive aquí para reusar registeredEntity hasta que haya un helper de siembra común (Q-74).
func TestPeriodStatusOfTheCalendar(t *testing.T) {
	entity, _, _ := registeredEntity(t)
	for d, want := range map[calendar.Date]calendar.PeriodStatus{
		{Year: 2025, Month: time.June, Day: 15}:  calendar.PeriodClosed,
		{Year: 2025, Month: time.May, Day: 15}:   calendar.PeriodClosing,
		{Year: 2025, Month: time.August, Day: 1}: calendar.PeriodOpen,
	} {
		if got, err := calendar.New().PeriodStatus(t.Context(), testPool, entity, d); err != nil || got != want {
			t.Errorf("PeriodStatus(%v) = %q, %v; se esperaba %q", d, got, err, want)
		}
	}
}
