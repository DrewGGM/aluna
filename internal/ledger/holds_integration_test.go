//go:build integration

package ledger_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// fundedDeposit crea un depósito de cliente (pasivo) con saldo natural 100 y la cuenta de clearing
// (pasivo) a la que irán sus capturas.
func fundedDeposit(t *testing.T) (deposit, clearing ledger.Account) {
	t.Helper()
	cash := createAccount(t, ledger.KindAsset)
	deposit, clearing = createAccount(t, ledger.KindLiability), createAccount(t, ledger.KindLiability)
	mustPostRef(t, transfer(t, cash.ID, deposit.ID, ids.New().String()))
	return deposit, clearing
}

func inTx[T any](t *testing.T, fn func(tx postgres.Tx) (T, error)) (T, error) { //nolint:ireturn // T es un tipo concreto en cada llamada
	t.Helper()
	var out T
	err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		var err error
		out, err = fn(tx)
		return err
	})
	return out, err
}

func placeHold(t *testing.T, account, counter ids.ID, units int64, expiresIn time.Duration) (ledger.Hold, error) {
	t.Helper()
	return inTx(t, func(tx postgres.Tx) (ledger.Hold, error) {
		return svc().PlaceHold(t.Context(), tx, ledger.PlaceHoldInput{
			LegalEntityID: entity, AccountID: account, CounterAccountID: counter,
			Amount: money.New(units, cop(t)), ExpiresAt: time.Now().Add(expiresIn),
		})
	})
}

type captured struct {
	hold ledger.Hold
	tx   ledger.Transaction
}

func captureInput(t *testing.T, hold ids.ID, units int64) ledger.CaptureInput {
	t.Helper()
	return ledger.CaptureInput{
		LegalEntityID: entity, HoldID: hold, Amount: money.New(units, cop(t)), BusinessDate: time.Now(), Actor: "tester", Channel: "test",
	}
}

func captureHold(t *testing.T, hold ids.ID, units int64) (captured, error) {
	t.Helper()
	return inTx(t, func(tx postgres.Tx) (captured, error) {
		h, posted, err := svc().CaptureHold(t.Context(), tx, captureInput(t, hold, units))
		return captured{h, posted}, err
	})
}

func releaseHold(t *testing.T, hold ids.ID) (ledger.Hold, error) {
	t.Helper()
	return inTx(t, func(tx postgres.Tx) (ledger.Hold, error) {
		return svc().ReleaseHold(t.Context(), tx, ledger.ReleaseInput{LegalEntityID: entity, HoldID: hold})
	})
}

func balanceOf(t *testing.T, account ids.ID) ledger.Balance {
	t.Helper()
	b, err := svc().Balance(t.Context(), testPool, account)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestAHoldReducesTheAvailable: un hold de 70 sobre un depósito de 100 deja 30 disponibles: no cabe un
// retiro de 100 ni otro hold de 31, y uno de 30 sí. El hold suma a pending_debits (el lado que reduce
// un pasivo).
func TestAHoldReducesTheAvailable(t *testing.T) {
	deposit, clearing := fundedDeposit(t)

	hold, err := placeHold(t, deposit.ID, clearing.ID, 70, time.Hour)

	if err != nil || hold.Status() != ledger.HoldPending || hold.Amount().Units() != 70 {
		t.Fatalf("PlaceHold = %+v, %v", hold, err)
	}
	if b := balanceOf(t, deposit.ID); b.PendingDebits != 70 || b.PendingCredits != 0 || b.Posted != -100 {
		t.Fatalf("saldo con el hold = %+v, se esperaban 70 pendientes de débito y el posted intacto", b)
	}
	if _, _, err := postRef(t, transfer(t, deposit.ID, clearing.ID, ids.New().String())); !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("retirar 100 con 70 retenidos = %v, se esperaba ErrInsufficientFunds", err)
	}
	if _, err := placeHold(t, deposit.ID, clearing.ID, 31, time.Hour); !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("otro hold de 31 = %v, se esperaba ErrInsufficientFunds", err)
	}
	if _, err := placeHold(t, deposit.ID, clearing.ID, 30, time.Hour); err != nil {
		t.Fatalf("otro hold de 30 = %v, cabe exacto", err)
	}
}

// TestAHoldOnAnAssetUsesPendingCredits: en una cuenta de naturaleza débito el dinero sale por el
// crédito, así que el hold suma a pending_credits.
func TestAHoldOnAnAssetUsesPendingCredits(t *testing.T) {
	cash, funding := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	mustPostRef(t, transfer(t, cash.ID, funding.ID, ids.New().String())) // caja con 100

	if _, err := placeHold(t, cash.ID, funding.ID, 60, time.Hour); err != nil {
		t.Fatal(err)
	}
	if b := balanceOf(t, cash.ID); b.PendingCredits != 60 || b.PendingDebits != 0 {
		t.Fatalf("saldo = %+v, se esperaban 60 pendientes de crédito", b)
	}
}

// TestCaptureFromAnAssetCreditsIt: el capture saca lo capturado de un activo por el crédito y libera
// su pending_credits.
func TestCaptureFromAnAssetCreditsIt(t *testing.T) {
	cash, funding := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	mustPostRef(t, transfer(t, cash.ID, funding.ID, ids.New().String())) // caja con 100
	hold, err := placeHold(t, cash.ID, funding.ID, 60, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := captureHold(t, hold.ID(), 60); err != nil {
		t.Fatal(err)
	}
	if b := balanceOf(t, cash.ID); b.Posted != 40 || b.PendingCredits != 0 {
		t.Fatalf("caja tras capturar 60 = %+v, se esperaba posted 40 y nada pendiente", b)
	}
	if b := balanceOf(t, funding.ID); b.Posted != -40 {
		t.Fatalf("contrapartida = %+v, se esperaba -40", b)
	}
}

// TestCaptureIntoAnAsyncCounterparty: la contrapartida puede ser async; el capture no la bloquea ni
// toca su saldo, que llevará el agregador, pero su entry queda asentado.
func TestCaptureIntoAnAsyncCounterparty(t *testing.T) {
	deposit, _ := fundedDeposit(t)
	clearing := asyncAccount(t, ledger.KindLiability)
	hold, err := placeHold(t, deposit.ID, clearing.ID, 30, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	got, err := captureHold(t, hold.ID(), 30)
	if err != nil {
		t.Fatal(err)
	}
	if b := balanceOf(t, deposit.ID); b.Posted != -70 || b.PendingDebits != 0 {
		t.Fatalf("depósito = %+v, se esperaba posted -70 y nada pendiente", b)
	}
	if b := balanceOf(t, clearing.ID); b.Posted != 0 {
		t.Fatalf("saldo materializado de la contrapartida async = %+v, se esperaba intacto", b)
	}
	checkCaptureTransaction(t, hold, got.tx)
}

// TestARejectedCaptureWritesNothing: aunque quien llama confirme la transacción tras el error, un
// capture que el posting rechaza no deja el pendiente liberado ni el hold cerrado.
func TestARejectedCaptureWritesNothing(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	hold, err := placeHold(t, deposit.ID, clearing.ID, 50, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	in := captureInput(t, hold.ID(), 50)
	in.Actor = ""
	var captureErr error
	err = postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		_, _, captureErr = svc().CaptureHold(t.Context(), tx, in)
		return nil
	})
	if err != nil || !errors.Is(captureErr, ledger.ErrInvalidPosting) {
		t.Fatalf("capture sin actor = %v (commit: %v), se esperaba ErrInvalidPosting", captureErr, err)
	}

	if b := balanceOf(t, deposit.ID); b.PendingDebits != 50 || b.Posted != -100 {
		t.Fatalf("saldo = %+v, se esperaba el hold aún retenido y el posted intacto", b)
	}
	if _, err := captureHold(t, hold.ID(), 50); err != nil {
		t.Fatalf("el hold debe seguir pending: %v", err)
	}
}

// TestCaptureAssentsAndReleasesTheRest: un capture parcial asienta lo capturado del origen a la
// contrapartida, libera el resto y deja el hold capturado con su transacción.
func TestCaptureAssentsAndReleasesTheRest(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	hold, err := placeHold(t, deposit.ID, clearing.ID, 70, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	got, err := captureHold(t, hold.ID(), 40)

	if err != nil || got.hold.Status() != ledger.HoldCaptured || got.hold.Captured().Units() != 40 {
		t.Fatalf("CaptureHold = %+v, %v", got.hold, err)
	}
	if b := balanceOf(t, deposit.ID); b.Posted != -60 || b.PendingDebits != 0 {
		t.Fatalf("depósito tras capturar 40 = %+v, se esperaba posted -60 y nada pendiente", b)
	}
	if b := balanceOf(t, clearing.ID); b.Posted != -40 {
		t.Fatalf("clearing = %+v, se esperaba -40", b)
	}
	checkCaptureTransaction(t, hold, got.tx)
	if _, err := captureHold(t, hold.ID(), 10); !errors.Is(err, ledger.ErrHoldNotPending) {
		t.Fatalf("capturar otra vez = %v, se esperaba ErrHoldNotPending", err)
	}
}

// checkCaptureTransaction verifica que la transacción del capture esté guardada con la fuente y la
// referencia del hold.
func checkCaptureTransaction(t *testing.T, hold ledger.Hold, tx ledger.Transaction) {
	t.Helper()
	stored, err := svc().Transaction(t.Context(), testPool, entity, tx.ID)
	if err != nil || len(stored.Entries) != 2 || stored.Source != "ledger.hold" || stored.ExternalRef != hold.ID().String() {
		t.Fatalf("transacción del capture = %+v, %v", stored.TransactionRecord, err)
	}
}

func TestReleaseRestoresTheAvailable(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	hold, err := placeHold(t, deposit.ID, clearing.ID, 70, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	released, err := releaseHold(t, hold.ID())

	if err != nil || released.Status() != ledger.HoldReleased {
		t.Fatalf("ReleaseHold = %+v, %v", released, err)
	}
	if b := balanceOf(t, deposit.ID); b.PendingDebits != 0 || b.Posted != -100 {
		t.Fatalf("saldo tras liberar = %+v", b)
	}
	if _, err := captureHold(t, hold.ID(), 10); !errors.Is(err, ledger.ErrHoldNotPending) {
		t.Fatalf("capturar uno liberado = %v, se esperaba ErrHoldNotPending", err)
	}
}

// TestCaptureRejectionsLeaveTheHoldPending: un capture de más o tras el vencimiento no toca nada. El
// hold vencido se inserta directo, sin esperar a que venza, y con su pendiente, como lo deja PlaceHold:
// así la base sigue cuadrada para la expiración de otras pruebas.
func TestCaptureRejectionsLeaveTheHoldPending(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	hold, err := placeHold(t, deposit.ID, clearing.ID, 70, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := captureHold(t, hold.ID(), 71); !errors.Is(err, ledger.ErrAmountExceedsHold) {
		t.Fatalf("capturar de más = %v, se esperaba ErrAmountExceedsHold", err)
	}
	expired := ids.New()
	if _, err := testPool.Exec(t.Context(), `
		WITH h AS (
			INSERT INTO ledger.holds (id, legal_entity_id, account_id, counter_account_id, currency, amount, expires_at)
			VALUES ($1, $2, $3, $4, 'COP', 10, clock_timestamp() - interval '1 second')
			RETURNING account_id, amount)
		UPDATE ledger.balances b SET pending_debits = b.pending_debits + h.amount, version = b.version + 1
		FROM h WHERE b.account_id = h.account_id`,
		expired, entity, deposit.ID, clearing.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := captureHold(t, expired, 10); !errors.Is(err, ledger.ErrHoldExpired) {
		t.Fatalf("capturar vencido = %v, se esperaba ErrHoldExpired", err)
	}
	if b := balanceOf(t, deposit.ID); b.PendingDebits != 80 || b.Posted != -100 {
		t.Fatalf("saldo = %+v, se esperaban los dos holds pendientes y el posted intacto", b)
	}
}

func TestPlaceHoldRejects(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	async := asyncAccount(t, ledger.KindLiability)
	cases := map[string]struct {
		account, counter ids.ID
		want             error
	}{
		"cuenta inexistente":        {ids.New(), clearing.ID, ledger.ErrAccountNotFound},
		"contrapartida inexistente": {deposit.ID, ids.New(), ledger.ErrAccountNotFound},
		"cuenta async":              {async.ID, clearing.ID, ledger.ErrInvalidHold},
		"la misma cuenta":           {deposit.ID, deposit.ID, ledger.ErrInvalidHold},
		"cuenta de otra entidad":    {accountOf(t, ids.New(), ledger.KindLiability).ID, clearing.ID, ledger.ErrAccountNotFound},
	}
	for name, tc := range cases {
		if _, err := placeHold(t, tc.account, tc.counter, 10, time.Hour); !errors.Is(err, tc.want) {
			t.Errorf("%s = %v, se esperaba %v", name, err, tc.want)
		}
	}
	if _, err := placeHold(t, deposit.ID, clearing.ID, 10, -time.Hour); !errors.Is(err, ledger.ErrInvalidHold) {
		t.Errorf("hold ya vencido = %v, se esperaba ErrInvalidHold", err)
	}
	if b := balanceOf(t, deposit.ID); b.PendingDebits != 0 {
		t.Fatalf("pendiente = %d, se esperaba 0: ningún hold rechazado retiene", b.PendingDebits)
	}
}

// TestTheHoldSourceIsReserved: un posting cualquiera no puede ocupar la referencia del capture de un
// hold (source ledger.hold) ni ninguna otra fuente ledger.*.
func TestTheHoldSourceIsReserved(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	hold, err := placeHold(t, deposit.ID, clearing.ID, 50, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	squatter := transfer(t, clearing.ID, deposit.ID, hold.ID().String())
	squatter.Source = "ledger.hold"

	if _, _, err := postRef(t, squatter); !errors.Is(err, ledger.ErrInvalidPosting) {
		t.Fatalf("posting con la fuente ledger.hold = %v, se esperaba ErrInvalidPosting", err)
	}
	if _, err := captureHold(t, hold.ID(), 50); err != nil {
		t.Fatalf("el capture debe pasar: %v", err)
	}
}

func TestHoldsOfAnotherEntityAreNotFound(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	hold, err := placeHold(t, deposit.ID, clearing.ID, 50, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, err = inTx(t, func(tx postgres.Tx) (ledger.Hold, error) {
		return svc().ReleaseHold(t.Context(), tx, ledger.ReleaseInput{LegalEntityID: ids.New(), HoldID: hold.ID()})
	})
	if !errors.Is(err, ledger.ErrHoldNotFound) {
		t.Fatalf("liberar el hold de otra entidad = %v, se esperaba ErrHoldNotFound", err)
	}
}

// TestTheHoldsTableEnforcesItsInvariants: aun con SQL directo y el rol de la aplicación, un hold
// terminado no cambia, su importe y sus cuentas tampoco, no se borra, no nace terminado y solo uno
// captured tiene transacción e importe capturado, distinta de la de otro hold.
func TestTheHoldsTableEnforcesItsInvariants(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	released, err := placeHold(t, deposit.ID, clearing.ID, 10, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := releaseHold(t, released.ID()); err != nil {
		t.Fatal(err)
	}
	pending, err := placeHold(t, deposit.ID, clearing.ID, 10, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	captured, err := placeHold(t, deposit.ID, clearing.ID, 10, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := captureHold(t, captured.ID(), 10); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		sql      string
		hold     ids.ID
		sqlState string
	}{
		"reabrir un hold terminado":           {`UPDATE ledger.holds SET status = 'pending' WHERE id = $1`, released.ID(), "2F003"},
		"cambiar el importe":                  {`UPDATE ledger.holds SET amount = 1 WHERE id = $1`, released.ID(), "42501"},
		"borrar":                              {`DELETE FROM ledger.holds WHERE id = $1`, released.ID(), "42501"},
		"liberar con transacción":             {`UPDATE ledger.holds SET status = 'released', capture_tx_id = gen_random_uuid() WHERE id = $1`, pending.ID(), "23514"},
		"liberar con importe capturado":       {`UPDATE ledger.holds SET status = 'released', captured_amount = 1 WHERE id = $1`, pending.ID(), "23514"},
		"capturar sin importe":                {`UPDATE ledger.holds SET status = 'captured', capture_tx_id = gen_random_uuid() WHERE id = $1`, pending.ID(), "23514"},
		"capturar con la transacción de otro": {`UPDATE ledger.holds SET status = 'captured', captured_amount = 1, capture_tx_id = (SELECT h.capture_tx_id FROM ledger.holds h WHERE h.account_id = holds.account_id AND h.status = 'captured') WHERE id = $1`, pending.ID(), "23505"},
		"insertar un hold ya capturado":       {`INSERT INTO ledger.holds (id, legal_entity_id, account_id, counter_account_id, currency, amount, expires_at, status) SELECT gen_random_uuid(), legal_entity_id, account_id, counter_account_id, currency, amount, expires_at, 'captured' FROM ledger.holds WHERE id = $1`, pending.ID(), "42501"},
	}
	for name, tc := range cases {
		_, err := testPool.Exec(t.Context(), tc.sql, tc.hold)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != tc.sqlState {
			t.Errorf("%s = %v, se esperaba SQLSTATE %s", name, err, tc.sqlState)
		}
	}
}

// TestASecondCaptureWaitsForTheFirst (AB-3): el segundo capture espera el lock del hold que tiene el
// primero y, cuando este confirma, ve el hold capturado: ErrHoldNotPending, sin tocar los saldos.
func TestASecondCaptureWaitsForTheFirst(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	hold, err := placeHold(t, deposit.ID, clearing.ID, 50, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pid, commitFirst := captureUncommitted(t, hold.ID())
	second := make(chan error, 1)
	go func() { _, err := captureHold(t, hold.ID(), 50); second <- err }()
	waitForBlockedBy(t, pid)

	if err := commitFirst(); err != nil {
		t.Fatal(err)
	}
	if err := <-second; !errors.Is(err, ledger.ErrHoldNotPending) {
		t.Fatalf("el segundo capture = %v, se esperaba ErrHoldNotPending", err)
	}
	if b := balanceOf(t, deposit.ID); b.Posted != -50 || b.PendingDebits != 0 {
		t.Fatalf("saldo = %+v, se esperaba posted -50 y nada pendiente: se asentó una vez", b)
	}
}

// captureUncommitted captura el hold completo en una transacción que queda abierta y devuelve el pid
// de su sesión y la función que la confirma.
func captureUncommitted(t *testing.T, hold ids.ID) (int32, func() error) {
	t.Helper()
	pids, done, release := make(chan int32, 1), make(chan error, 1), make(chan struct{})
	t.Cleanup(sync.OnceFunc(func() { close(release) }))
	go func() {
		done <- postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
			if _, _, err := svc().CaptureHold(t.Context(), tx, captureInput(t, hold, 50)); err != nil {
				return err
			}
			var pid int32
			if err := tx.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return err
			}
			pids <- pid
			<-release
			return nil
		})
	}()
	select {
	case pid := <-pids:
		return pid, func() error { release <- struct{}{}; return <-done }
	case err := <-done:
		t.Fatalf("el primer capture falló: %v", err)
		return 0, nil
	}
}
