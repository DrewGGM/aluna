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
	"github.com/DrewGGM/aluna/internal/postgres"
)

func aggregate(t *testing.T, batch int) int {
	t.Helper()
	n, err := svc.AggregateAsyncBalances(t.Context(), postgres.NewUnitOfWork(testPool), batch)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// unaggregated cuenta las cuentas async cuyo saldo no es la suma de sus entries.
func unaggregated(t *testing.T) int {
	t.Helper()
	var n int
	err := testPool.QueryRow(t.Context(), `
		SELECT count(*) FROM ledger.accounts a JOIN ledger.balances b ON b.account_id = a.id
		WHERE a.balance_mode = 'async'
		  AND b.posted <> coalesce((SELECT sum(e.amount) FROM ledger.entries e WHERE e.account_id = a.id), 0)`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// catchUp agrega hasta que todas las cuentas async cuadran. El agregador solo toma lo que asentaron
// transacciones anteriores a la más vieja en curso en el servidor, y en CI el servidor lo comparten
// otros paquetes: puede necesitar más de una corrida.
func catchUp(t *testing.T, batch int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for aggregate(t, batch); unaggregated(t) > 0; aggregate(t, batch) {
		if time.Now().After(deadline) {
			t.Fatalf("tras 10 s quedan %d cuentas async sin cuadrar", unaggregated(t))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestPostDoesNotCheckNorUpdateAsyncAccounts: un posting saca de una cuenta async más de lo que tiene
// sin que se valide su límite, y su saldo materializado no cambia hasta que corre el agregador, que no
// toca el de la cuenta sync. Que Post no la bloquea lo prueba TestPostSkipsAsyncAccounts, en app.
func TestPostDoesNotCheckNorUpdateAsyncAccounts(t *testing.T) {
	cash, settlement := createAccount(t, ledger.KindAsset), asyncAccount(t, ledger.KindAsset)

	mustPostRef(t, transfer(t, cash.ID, settlement.ID, ids.New().String())) // el activo async queda en -100

	if b := balanceOf(t, settlement.ID); b.Posted != 0 || b.Version != 0 {
		t.Fatalf("saldo de la cuenta async antes de agregar = %+v, se esperaba intacto", b)
	}
	catchUp(t, 100)
	if b := balanceOf(t, settlement.ID); b.Posted != -100 {
		t.Fatalf("saldo de la cuenta async tras agregar = %+v, se esperaba -100", b)
	}
	if b := balanceOf(t, cash.ID); b.Posted != 100 {
		t.Fatalf("caja (sync) tras agregar = %+v, se esperaba 100: el agregador solo toca cuentas async", b)
	}
}

// TestAggregationMatchesTheEntries: tras agregar en lotes chicos, el saldo de cada cuenta async es la
// suma de sus entries, y una segunda corrida no encuentra nada ni duplica.
func TestAggregationMatchesTheEntries(t *testing.T) {
	clearing, income := asyncAccount(t, ledger.KindLiability), asyncAccount(t, ledger.KindIncome)
	for range 3 {
		deposit, _ := fundedDeposit(t)
		mustPostRef(t, transfer(t, deposit.ID, clearing.ID, ids.New().String()))
		mustPostRef(t, transfer(t, clearing.ID, income.ID, ids.New().String()))
	}

	catchUp(t, 2)
	before := balanceOf(t, clearing.ID)

	if n := aggregate(t, 2); n != 0 {
		t.Fatalf("la segunda corrida agregó %d entries, se esperaba 0", n)
	}
	if after := balanceOf(t, clearing.ID); after != before || unaggregated(t) != 0 {
		t.Fatalf("saldo tras agregar otra vez = %+v, antes %+v: se duplicó", after, before)
	}
	if b := balanceOf(t, income.ID); b.Posted != -300 {
		t.Fatalf("ingresos = %+v, se esperaba -300", b)
	}
}

// TestAPostingInFlightIsNotSkipped: un posting que tomó su id de entry antes que otro pero confirma
// después no queda atrás del cursor: el agregador no pasa de la transacción más vieja en curso.
func TestAPostingInFlightIsNotSkipped(t *testing.T) {
	clearing := asyncAccount(t, ledger.KindLiability)
	first, second := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindAsset)
	_, finishFirst := postAndHold(t, transfer(t, first.ID, clearing.ID, ids.New().String()))
	mustPostRef(t, transfer(t, second.ID, clearing.ID, ids.New().String()))

	aggregate(t, 100)
	if _, err := finishFirst(true); err != nil {
		t.Fatal(err)
	}

	catchUp(t, 100)
	if b := balanceOf(t, clearing.ID); b.Posted != -200 {
		t.Fatalf("clearing = %+v, se esperaban los dos postings (-200)", b)
	}
}

// TestAnOlderTransactionWithNewerEntriesIsAggregated: una transacción de base que empezó antes (xid
// menor) pero asienta sus entries después que otra tiene ids mayores; el cursor sigue el orden de los
// xid y no la deja atrás.
func TestAnOlderTransactionWithNewerEntriesIsAggregated(t *testing.T) {
	clearing := asyncAccount(t, ledger.KindLiability)
	older, newer := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindAsset)
	started, release, done := make(chan error, 1), make(chan struct{}), make(chan error, 1)
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock) // si la prueba falla antes, la transacción no queda abierta frenando al agregador
	go func() {
		done <- postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
			_, err := tx.Exec(t.Context(), `SELECT pg_current_xact_id()`) // toma su xid ya
			started <- err
			<-release
			_, err = svc.Post(t.Context(), tx, transfer(t, older.ID, clearing.ID, ids.New().String()))
			return err
		})
	}()
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	mustPostRef(t, transfer(t, newer.ID, clearing.ID, ids.New().String()))
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	catchUp(t, 1)
	if b := balanceOf(t, clearing.ID); b.Posted != -200 {
		t.Fatalf("clearing = %+v, se esperaban los dos postings (-200)", b)
	}
}

// TestTheCursorOnlyMovesForward: ni con SQL directo el rol de la aplicación retrocede el cursor ni
// borra su fila.
func TestTheCursorOnlyMovesForward(t *testing.T) {
	cases := map[string]struct{ sql, sqlState string }{
		"retroceder": {`UPDATE ledger.async_cursor SET posted_xact = '0', entry_id = 0`, "2F003"},
		"borrar":     {`DELETE FROM ledger.async_cursor`, "42501"},
	}
	catchUp(t, 100) // el cursor ya avanzó sobre los entries de las cuentas async de este paquete
	for name, tc := range cases {
		_, err := testPool.Exec(t.Context(), tc.sql)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != tc.sqlState {
			t.Errorf("%s = %v, se esperaba SQLSTATE %s", name, err, tc.sqlState)
		}
	}
}

func TestAggregateRejectsAnEmptyBatch(t *testing.T) {
	if _, err := svc.AggregateAsyncBalances(t.Context(), postgres.NewUnitOfWork(testPool), 0); err == nil {
		t.Fatal("un lote de 0 debe fallar: no avanzaría nunca")
	}
}
