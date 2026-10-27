//go:build integration

package ledger_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// transfer es un posting de 100 COP de from (crédito) a to (débito) con la referencia ref.
func transfer(t *testing.T, to, from ids.ID, ref string) ledger.PostInput {
	t.Helper()
	today := time.Now().UTC().Truncate(24 * time.Hour)
	return ledger.PostInput{
		LegalEntityID: entity, Code: "test", ValueDate: today, BusinessDate: today,
		Source: "test", ExternalRef: ref, Actor: "tester", Channel: "test", RuleVersion: "v1",
		Entries: []ledger.EntryInput{
			{AccountID: to, Amount: money.New(100, cop(t))},
			{AccountID: from, Amount: money.New(-100, cop(t))},
		},
	}
}

// postRef asienta in en su propia transacción como lo haría un productor interno: un repetido se toma
// como hecho y se confirma. No llama t.Fatal, así que sirve desde otras goroutines; err es el de Post
// (si no es un repetido) o el del commit.
func postRef(t *testing.T, in ledger.PostInput) (posted ledger.Transaction, duplicate bool, err error) {
	t.Helper()
	err = postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		var postErr error
		posted, postErr = svc().Post(t.Context(), tx, in)
		if duplicate = errors.Is(postErr, ledger.ErrDuplicateExternalRef); duplicate {
			return nil
		}
		return postErr
	})
	return posted, duplicate, err
}

func mustPostRef(t *testing.T, in ledger.PostInput) (ledger.Transaction, bool) {
	t.Helper()
	posted, duplicate, err := postRef(t, in)
	if err != nil {
		t.Fatal(err)
	}
	return posted, duplicate
}

// countByRef cuenta las transacciones de la entidad con la referencia ref y sus entries.
func countByRef(t *testing.T, ref string) (transactions, entries int) {
	t.Helper()
	err := testPool.QueryRow(t.Context(), `SELECT count(DISTINCT t.id), count(e.id) FROM ledger.transactions t
		JOIN ledger.entries e ON e.transaction_id = t.id WHERE t.legal_entity_id = $1 AND t.external_ref = $2`,
		entity, ref).Scan(&transactions, &entries)
	if err != nil {
		t.Fatal(err)
	}
	return transactions, entries
}

// TestARepeatedExternalRefReturnsTheOriginal: el segundo Post con el mismo (entidad, source,
// external_ref) devuelve la transacción original como repetida y no escribe nada, aunque ya no cabría
// (el productor reintenta algo que ya pasó).
func TestARepeatedExternalRefReturnsTheOriginal(t *testing.T) {
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	ref := ids.New().String()
	if _, duplicate := mustPostRef(t, transfer(t, cash.ID, deposit.ID, ids.New().String())); duplicate { // fondea la caja
		t.Fatal("el fondeo no es repetido")
	}
	original, duplicate := mustPostRef(t, transfer(t, deposit.ID, cash.ID, ref)) // vacía la caja
	if duplicate {
		t.Fatal("el primer Post no es repetido")
	}

	again, duplicate := mustPostRef(t, transfer(t, deposit.ID, cash.ID, ref)) // ya no habría fondos

	if !duplicate || again.ID != original.ID || !again.PostedAt.Equal(original.PostedAt) || len(again.Entries) != 2 {
		t.Fatalf("segundo Post = %+v (repetido: %v), se esperaba la original %s", again, duplicate, original.ID)
	}
	if txs, entries := countByRef(t, ref); txs != 1 || entries != 2 {
		t.Fatalf("con la referencia hay %d transacciones y %d entries, se esperaban 1 y 2", txs, entries)
	}
	if got := postedOf(t, cash.ID); got != 0 {
		t.Fatalf("saldo de la caja = %d, se esperaba 0: el repetido no mueve dinero", got)
	}
}

// TestConcurrentPostsWithTheSameRefPostOnce: 10 productores a la vez con la misma referencia asientan
// una sola transacción; los demás reciben esa misma como repetida, nunca un error.
func TestConcurrentPostsWithTheSameRefPostOnce(t *testing.T) {
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	ref := ids.New().String()
	posted, duplicates, errs := make([]ledger.Transaction, 10), make([]bool, 10), make([]error, 10)
	var wg sync.WaitGroup
	for i := range posted {
		wg.Go(func() { posted[i], duplicates[i], errs[i] = postRef(t, transfer(t, cash.ID, deposit.ID, ref)) })
	}
	wg.Wait()

	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	fresh := 0
	for i, p := range posted {
		if !duplicates[i] {
			fresh++
		}
		if p.ID != posted[0].ID {
			t.Errorf("productor %d recibió %s, se esperaba %s", i, p.ID, posted[0].ID)
		}
	}
	if txs, entries := countByRef(t, ref); fresh != 1 || txs != 1 || entries != 2 {
		t.Fatalf("%d productores asentaron; hay %d transacciones y %d entries; se esperaban 1, 1 y 2", fresh, txs, entries)
	}
}

func TestTheSameRefFromAnotherSourceIsAnotherTransaction(t *testing.T) {
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	in := transfer(t, cash.ID, deposit.ID, ids.New().String())
	first, _ := mustPostRef(t, in)
	in.Source = "otra-fuente"

	if second, duplicate := mustPostRef(t, in); duplicate || second.ID == first.ID {
		t.Fatalf("la misma referencia de otra fuente = %s (repetido: %v); se esperaba otra transacción", second.ID, duplicate)
	}
}

// TestTheSameRefInAnotherEntityIsAnotherTransaction: la unicidad es por entidad legal, también cuando
// las dos entidades ya tienen la referencia.
func TestTheSameRefInAnotherEntityIsAnotherTransaction(t *testing.T) {
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	ref := ids.New().String()
	first, _ := mustPostRef(t, transfer(t, cash.ID, deposit.ID, ref))
	other := ids.New()
	otherCash := accountOf(t, other, ledger.KindAsset)
	otherDeposit := accountOf(t, other, ledger.KindLiability)
	in := transfer(t, otherCash.ID, otherDeposit.ID, ref)
	in.LegalEntityID = other

	second, duplicate := mustPostRef(t, in)
	if duplicate || second.ID == first.ID {
		t.Fatalf("la misma referencia en otra entidad = %s (repetido: %v); se esperaba otra transacción", second.ID, duplicate)
	}
	if again, duplicate := mustPostRef(t, in); !duplicate || again.ID != second.ID {
		t.Fatalf("repetir en la otra entidad = %s (repetido: %v); se esperaba su original %s", again.ID, duplicate, second.ID)
	}
}

func accountOf(t *testing.T, legalEntity ids.ID, kind ledger.Kind) ledger.Account {
	t.Helper()
	a, err := tryCreateAccount(t, ledger.CreateAccountInput{
		LegalEntityID: legalEntity, Code: "cta-" + ids.New().String(), Kind: kind, Currency: cop(t), Mode: ledger.BalanceModeSync,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// postAndHold asienta in en una transacción que queda abierta y devuelve el pid de su sesión y una
// función que la termina (con commit o con rollback) y devuelve lo asentado.
func postAndHold(t *testing.T, in ledger.PostInput) (int32, func(commit bool) (ledger.Transaction, error)) {
	t.Helper()
	pids, done, release := make(chan int32, 1), make(chan error, 1), make(chan bool, 1)
	t.Cleanup(sync.OnceFunc(func() { release <- false })) // si la prueba falla antes, la transacción no queda abierta
	var posted ledger.Transaction
	go func() {
		done <- postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
			var err error
			if posted, err = svc().Post(t.Context(), tx, in); err != nil {
				return err
			}
			var pid int32
			if err := tx.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return err
			}
			pids <- pid
			if !<-release {
				return errors.New("la primera se revierte")
			}
			return nil
		})
	}()
	select {
	case pid := <-pids:
		return pid, func(commit bool) (ledger.Transaction, error) {
			release <- commit
			err := <-done
			return posted, err
		}
	case err := <-done:
		t.Fatalf("el primer productor no asentó: %v", err)
		return 0, nil
	}
}

// raceAgainst asienta second mientras first sigue abierto, espera a que second quede bloqueado por
// first, termina first (commit o rollback) y devuelve lo que obtuvo second.
func raceAgainst(t *testing.T, first, second ledger.PostInput, commit bool) (firstTx, secondTx ledger.Transaction, duplicate bool) {
	t.Helper()
	pid, finish := postAndHold(t, first)
	type result struct {
		posted    ledger.Transaction
		duplicate bool
		err       error
	}
	results := make(chan result, 1)
	go func() {
		var r result
		r.posted, r.duplicate, r.err = postRef(t, second)
		results <- r
	}()
	waitForBlockedBy(t, pid)

	firstTx, err := finish(commit)
	if (err == nil) != commit {
		t.Fatalf("la primera transacción terminó con %v", err)
	}
	r := <-results
	if r.err != nil {
		t.Fatalf("el segundo productor = %v", r.err)
	}
	return firstTx, r.posted, r.duplicate
}

// TestAProducerThatLosesTheRaceGetsTheOriginal: si otro productor asienta la misma referencia después
// de la búsqueda inicial, el segundo recibe la original como repetida y no escribe nada. Sobre las
// mismas cuentas espera los locks de los saldos y, con el commit del primero, ya no le alcanza: es el
// caso real de un reintento. Sobre otras cuentas espera el INSERT de la referencia.
func TestAProducerThatLosesTheRaceGetsTheOriginal(t *testing.T) {
	for name, sameAccounts := range map[string]bool{"mismas cuentas": true, "otras cuentas": false} {
		t.Run(name, func(t *testing.T) {
			cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
			if _, duplicate := mustPostRef(t, transfer(t, cash.ID, deposit.ID, ids.New().String())); duplicate { // fondea 100
				t.Fatal("el fondeo no es repetido")
			}
			ref := ids.New().String()
			firstIn := transfer(t, deposit.ID, cash.ID, ref) // vacía la caja
			secondIn := firstIn                              // el reintento: ya no le alcanza
			if !sameAccounts {
				otherCash, otherDeposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
				secondIn = transfer(t, otherCash.ID, otherDeposit.ID, ref) // fondea: solo espera el INSERT
			}

			first, second, duplicate := raceAgainst(t, firstIn, secondIn, true)

			if !duplicate || second.ID != first.ID {
				t.Fatalf("el segundo = %s (repetido: %v), se esperaba la original %s", second.ID, duplicate, first.ID)
			}
			if txs, entries := countByRef(t, ref); txs != 1 || entries != 2 {
				t.Fatalf("con la referencia hay %d transacciones y %d entries, se esperaban 1 y 2", txs, entries)
			}
		})
	}
}

// TestIfTheFirstRollsBackTheSecondPosts: si el primero que tenía la referencia se revierte, el segundo,
// que esperaba su INSERT, la asienta.
func TestIfTheFirstRollsBackTheSecondPosts(t *testing.T) {
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	otherCash, otherDeposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	ref := ids.New().String()

	_, second, duplicate := raceAgainst(t, transfer(t, cash.ID, deposit.ID, ref), transfer(t, otherCash.ID, otherDeposit.ID, ref), false)

	if duplicate || second.ID == (ids.ID{}) || postedOf(t, otherCash.ID) != 100 {
		t.Fatalf("el segundo = %s (repetido: %v); se esperaba que asentara", second.ID, duplicate)
	}
	if txs, _ := countByRef(t, ref); txs != 1 {
		t.Fatalf("con la referencia hay %d transacciones, se esperaba 1", txs)
	}
}
