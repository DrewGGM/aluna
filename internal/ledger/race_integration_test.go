//go:build integration

package ledger_test

import (
	"errors"
	"maps"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// debit debita units a account y acredita counterpart, en su propia transacción y con una referencia
// nueva. No llama t.Fatal, así que sirve desde otras goroutines.
func debit(t *testing.T, c money.Currency, account, counterpart ids.ID, units int64) error {
	t.Helper()
	return postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		_, err := move(t.Context(), tx, c, account, counterpart, units, ids.New().String())
		return err
	})
}

// concurrently corre fn n veces, soltando las goroutines juntas, y devuelve sus errores. Antes abre
// todas las conexiones del pool: si el pool tuviera que abrirlas en el momento, la primera transacción
// terminaría sola y no habría carrera que probar. Las transacciones que compiten a la vez son a lo
// sumo MaxConns del pool; las demás esperan conexión.
func concurrently(t *testing.T, n int, fn func(i int) error) []error {
	t.Helper()
	warmUp(t)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() {
			<-start
			errs[i] = fn(i)
		})
	}
	close(start)
	wg.Wait()
	return errs
}

// warmUp abre todas las conexiones del pool y las devuelve.
func warmUp(t *testing.T) {
	t.Helper()
	if testPool.Config().MaxConns < 2 {
		t.Fatal("con una sola conexión en el pool no hay carrera que probar")
	}
	conns := make([]*pgxpool.Conn, 0, testPool.Config().MaxConns)
	defer func() {
		for _, conn := range conns {
			conn.Release()
		}
	}()
	for range cap(conns) {
		conn, err := testPool.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
	}
}

// netOfEntries suma, por moneda, los entries de todas las transacciones que tocan account.
func netOfEntries(t *testing.T, account ids.ID) map[string]int64 {
	t.Helper()
	rows, err := testPool.Query(t.Context(), `SELECT currency, sum(amount)::bigint FROM ledger.entries
		WHERE transaction_id IN (SELECT transaction_id FROM ledger.entries WHERE account_id = $1)
		GROUP BY currency`, account)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	net := map[string]int64{}
	for rows.Next() {
		var (
			currency string
			sum      int64
		)
		if err := rows.Scan(&currency, &sum); err != nil {
			t.Fatal(err)
		}
		net[currency] = sum
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return net
}

// outcomes cuenta los retiros que pasaron y los rechazados por fondos; cualquier otro error falla.
func outcomes(t *testing.T, errs []error) (succeeded, insufficient int) {
	t.Helper()
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ledger.ErrInsufficientFunds):
			insufficient++
		default:
			t.Errorf("retiro con un error inesperado: %v", err)
		}
	}
	return succeeded, insufficient
}

// TestRaceDebits (QS-1): 50 retiros de 70.000 compiten sobre un depósito de 100.000: exactamente uno
// pasa y los demás son fondos insuficientes. Sin el FOR UPDATE de los saldos, los que compiten leerían
// 100.000 a la vez y pasarían: el depósito quedaría sobregirado. La contrapartida es una cuenta async
// (no se bloquea ni se actualiza en el posting): con dos cuentas sync, sin el FOR UPDATE sus UPDATE se
// bloquearían en ciclo, el 40P01 se reintentaría con el saldo ya confirmado y ocultaría el sobregiro.
func TestRaceDebits(t *testing.T) {
	c := cop(t)
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	clearing, err := tryCreateAccount(t, ledger.CreateAccountInput{
		LegalEntityID: entity, Code: "puente-" + ids.New().String(), Kind: ledger.KindLiability, Currency: c,
		Mode: ledger.BalanceModeAsync,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := debit(t, c, cash.ID, deposit.ID, 10_000_000); err != nil { // depósito con saldo natural 100.000
		t.Fatal(err)
	}

	errs := concurrently(t, 50, func(int) error { return debit(t, c, deposit.ID, clearing.ID, 7_000_000) }) // retiros de 70.000

	if succeeded, insufficient := outcomes(t, errs); succeeded != 1 || insufficient != 49 {
		t.Fatalf("%d retiros pasaron y %d fueron fondos insuficientes; se esperaban 1 y 49", succeeded, insufficient)
	}
	if natural := ledger.NaturalBalance(ledger.KindLiability, postedOf(t, deposit.ID)); natural != 3_000_000 {
		t.Fatalf("saldo natural del depósito = %d, se esperaba 3.000.000 (30.000 COP)", natural)
	}
	if net := netOfEntries(t, deposit.ID); !maps.Equal(net, map[string]int64{"COP": 0}) {
		t.Fatalf("Σ entries por moneda = %v, se esperaba COP: 0", net)
	}
	var entries int64
	if err := testPool.QueryRow(t.Context(), `SELECT sum(amount)::bigint FROM ledger.entries WHERE account_id = $1`, deposit.ID).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if posted := postedOf(t, deposit.ID); entries != posted {
		t.Fatalf("Σ entries del depósito = %d y su saldo materializado = %d: deben coincidir", entries, posted)
	}
}

// TestRaceCrossTransfers: transferencias A→B y B→A a la vez terminan todas, sin deadlock ni espera
// que agote el lock_timeout, porque los saldos se bloquean en orden de cuenta (ADR-0004). El reintento
// de UnitOfWork ante un 40P01 lo prueba TestDeadlockIsRetried.
func TestRaceCrossTransfers(t *testing.T) {
	c := cop(t)
	a, b, funding := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	for _, account := range []ids.ID{a.ID, b.ID} {
		if err := debit(t, c, account, funding.ID, 1_000_000); err != nil {
			t.Fatal(err)
		}
	}

	errs := concurrently(t, 40, func(i int) error {
		if i%2 == 1 {
			return debit(t, c, a.ID, b.ID, 1_000)
		}
		return debit(t, c, b.ID, a.ID, 1_000)
	})

	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if pa, pb := postedOf(t, a.ID), postedOf(t, b.ID); pa != 1_000_000 || pb != 1_000_000 {
		t.Fatalf("saldos = %d y %d, se esperaban 1.000.000 y 1.000.000", pa, pb)
	}
}
