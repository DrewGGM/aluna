//go:build integration

package calendar_test

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

var (
	testPool *pgxpool.Pool
	cal      = calendar.New()
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, &testPool)) }

// newEntity registra una entidad legal con su fecha de negocio. Las crea quien opera el despliegue, no
// la aplicación (aluna_app no tiene INSERT): la prueba vuelve al dueño de las tablas.
func newEntity(t *testing.T, businessDate calendar.Date) ids.ID {
	t.Helper()
	id := ids.New()
	err := pgx.BeginFunc(t.Context(), testPool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), "SET LOCAL ROLE NONE"); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), `INSERT INTO calendar.legal_entities (id, name, timezone, business_date, functional_currency)
			VALUES ($1, 'Entidad de prueba', 'America/Bogota', $2, 'COP')`, id, dateOf(businessDate))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func dateOf(d calendar.Date) time.Time {
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC)
}

func rollover(t *testing.T, entity ids.ID, expected calendar.Date) (calendar.Date, error) {
	t.Helper()
	var next calendar.Date
	err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		var err error
		next, err = cal.Rollover(t.Context(), tx, entity, expected)
		return err
	})
	return next, err
}

func businessDate(t *testing.T, entity ids.ID) calendar.Date {
	t.Helper()
	d, err := cal.BusinessDate(t.Context(), testPool, entity)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestRolloverAdvancesToTheNextBusinessDay: del viernes 27 de junio de 2025 pasa al martes 1 de julio
// (sábado, domingo y el lunes 30, festivo, no son hábiles).
func TestRolloverAdvancesToTheNextBusinessDay(t *testing.T) {
	friday := calendar.Date{Year: 2025, Month: time.June, Day: 27}
	entity := newEntity(t, friday)

	next, err := rollover(t, entity, friday)

	want := calendar.Date{Year: 2025, Month: time.July, Day: 1}
	if err != nil || next != want {
		t.Fatalf("Rollover = %v, %v; se esperaba %v", next, err, want)
	}
	if got := businessDate(t, entity); got != want {
		t.Fatalf("fecha de negocio = %v, se esperaba %v", got, want)
	}
}

// TestARepeatedRolloverIsStale: el segundo rollover con la misma fecha esperada no avanza otra vez.
func TestARepeatedRolloverIsStale(t *testing.T) {
	monday := calendar.Date{Year: 2025, Month: time.July, Day: 7}
	entity := newEntity(t, monday)
	if _, err := rollover(t, entity, monday); err != nil {
		t.Fatal(err)
	}

	if _, err := rollover(t, entity, monday); !errors.Is(err, calendar.ErrStaleBusinessDate) {
		t.Fatalf("segundo rollover = %v, se esperaba ErrStaleBusinessDate", err)
	}
	if got, want := businessDate(t, entity), (calendar.Date{Year: 2025, Month: time.July, Day: 8}); got != want {
		t.Fatalf("fecha de negocio = %v, se esperaba %v: avanzó dos veces", got, want)
	}
}

func TestAnUnknownEntityHasNoBusinessDate(t *testing.T) {
	unknown := ids.New()
	if _, err := cal.BusinessDate(t.Context(), testPool, unknown); !errors.Is(err, calendar.ErrLegalEntityNotFound) {
		t.Errorf("BusinessDate = %v, se esperaba ErrLegalEntityNotFound", err)
	}
	if _, err := rollover(t, unknown, calendar.Date{Year: 2025, Month: time.July, Day: 7}); !errors.Is(err, calendar.ErrLegalEntityNotFound) {
		t.Errorf("Rollover = %v, se esperaba ErrLegalEntityNotFound", err)
	}
}

// TestTheBusinessDateOnlyMovesForward: aun con SQL directo, el rol de la aplicación no retrocede la
// fecha de negocio ni la deja igual ni la lleva a 'infinity', no crea ni borra entidades y no cambia más
// que la fecha.
func TestTheBusinessDateOnlyMovesForward(t *testing.T) {
	entity := newEntity(t, calendar.Date{Year: 2025, Month: time.July, Day: 7})
	cases := map[string]struct{ sql, sqlState string }{
		"retroceder":           {`UPDATE calendar.legal_entities SET business_date = '2025-07-04' WHERE id = $1`, "2F003"},
		"dejarla igual":        {`UPDATE calendar.legal_entities SET business_date = business_date WHERE id = $1`, "2F003"},
		"llevarla al infinito": {`UPDATE calendar.legal_entities SET business_date = 'infinity' WHERE id = $1`, "23514"},
		"cambiar la moneda":    {`UPDATE calendar.legal_entities SET functional_currency = 'USD' WHERE id = $1`, "42501"},
		"borrar":               {`DELETE FROM calendar.legal_entities WHERE id = $1`, "42501"},
		"crear otra entidad":   {`INSERT INTO calendar.legal_entities (id, name, timezone, business_date, functional_currency) SELECT gen_random_uuid(), name, timezone, business_date, functional_currency FROM calendar.legal_entities WHERE id = $1`, "42501"},
	}
	for name, tc := range cases {
		_, err := testPool.Exec(t.Context(), tc.sql, entity)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != tc.sqlState {
			t.Errorf("%s = %v, se esperaba SQLSTATE %s", name, err, tc.sqlState)
		}
	}
}

// TestAPostingKeepsTheDateItRead: una transacción lee el lunes y sigue abierta; el rollover a martes no
// la espera (lock_timeout de 2 s: si la lectura bloqueara la fila, fallaría), y la transacción asienta
// después con el lunes que leyó y confirma.
func TestAPostingKeepsTheDateItRead(t *testing.T) {
	monday, tuesday := calendar.Date{Year: 2025, Month: time.July, Day: 7}, calendar.Date{Year: 2025, Month: time.July, Day: 8}
	entity := newEntity(t, monday)
	cash, deposit := ledgerAccounts(t, entity)
	read, release, done := make(chan calendar.Date, 1), make(chan struct{}), make(chan error, 1)
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	var posted ids.ID
	go func() {
		done <- postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
			day, err := cal.BusinessDate(t.Context(), tx, entity)
			read <- day
			if err != nil {
				return err
			}
			<-release
			posted, err = postOn(t, tx, entity, cash, deposit, day)
			return err
		})
	}()
	if d := <-read; d != monday {
		t.Fatalf("la transacción leyó %v", d)
	}

	if _, err := rollover(t, entity, monday); err != nil {
		t.Fatalf("el rollover esperó a la transacción abierta: %v", err)
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	stored, err := ledger.New(cal).Transaction(t.Context(), testPool, entity, posted)
	if err != nil || !stored.BusinessDate.Equal(dateOf(monday)) {
		t.Fatalf("el posting quedó con %v (%v), se esperaba el lunes que leyó", stored.BusinessDate, err)
	}
	if got := businessDate(t, entity); got != tuesday {
		t.Fatalf("fecha de negocio = %v, se esperaba %v", got, tuesday)
	}
}

// TestRolloverDuringPostings (QS-2): 50 transacciones leen la fecha de negocio y asientan con ella mientras
// la fecha rueda, y ninguna falla. Unas leen el lunes y asientan después de que el rollover confirma (tantas
// como conexiones del pool menos una: cada una retiene la suya y el rollover necesita otra), el resto hasta
// la mitad corre a la vez que el rollover y la otra mitad empieza después; cada transacción queda con la
// fecha que leyó.
// Asientan con el ledger, el que lee la fecha en producción: la prueba es del contrato entre los dos
// módulos.
func TestRolloverDuringPostings(t *testing.T) {
	monday, tuesday := calendar.Date{Year: 2025, Month: time.July, Day: 7}, calendar.Date{Year: 2025, Month: time.July, Day: 8}
	entity := newEntity(t, monday)
	cash, deposit := ledgerAccounts(t, entity)
	posted, errs := make([]postedOn, 50), make([]error, 50)
	rolled, now := make(chan struct{}), make(chan struct{})
	close(now)
	var held, wg sync.WaitGroup
	start := func(i int, wait <-chan struct{}, read *sync.WaitGroup) {
		wg.Go(func() { posted[i], errs[i] = postAfter(t, entity, cash, deposit, wait, read) })
	}
	holders := min(10, int(testPool.Config().MaxConns)-1)
	for i := range holders {
		held.Add(1)
		start(i, rolled, &held)
	}
	held.Wait() // ya leyeron el lunes y esperan
	for i := holders; i < 30; i++ {
		start(i, now, nil)
	}
	if _, err := rollover(t, entity, monday); err != nil {
		t.Fatal(err)
	}
	close(rolled)
	for i := 30; i < 50; i++ {
		start(i, now, nil)
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatalf("postings que fallaron: %v", err)
	}
	checkPostedOnTheDateRead(t, entity, posted, map[calendar.Date]int{monday: holders, tuesday: 20})
}

type postedOn struct {
	tx   ids.ID
	read calendar.Date
}

// postAfter lee la fecha de negocio al empezar la transacción, avisa en read que la leyó, espera wait y
// asienta con ella.
func postAfter(t *testing.T, entity, cash, deposit ids.ID, wait <-chan struct{}, read *sync.WaitGroup) (postedOn, error) {
	t.Helper()
	var p postedOn
	err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		day, err := cal.BusinessDate(t.Context(), tx, entity)
		if read != nil {
			read.Done()
		}
		if err != nil {
			return err
		}
		<-wait
		p.read = day
		p.tx, err = postOn(t, tx, entity, cash, deposit, day)
		return err
	})
	return p, err
}

// checkPostedOnTheDateRead: cada transacción quedó con la fecha que leyó, y hay al menos atLeast[d]
// asentadas con cada fecha d.
func checkPostedOnTheDateRead(t *testing.T, entity ids.ID, posted []postedOn, atLeast map[calendar.Date]int) {
	t.Helper()
	seen := map[calendar.Date]int{}
	for _, p := range posted {
		stored, err := ledger.New(cal).Transaction(t.Context(), testPool, entity, p.tx)
		if err != nil || !stored.BusinessDate.Equal(dateOf(p.read)) {
			t.Fatalf("transacción %s: fecha de negocio %v, había leído %v (%v)", p.tx, stored.BusinessDate, p.read, err)
		}
		seen[p.read]++
	}
	for d, n := range atLeast {
		if seen[d] < n {
			t.Fatalf("%d transacciones con %v, se esperaban al menos %d (%v)", seen[d], d, n, seen)
		}
	}
}

// postOn asienta dentro de tx un depósito con la fecha de negocio day, como lo hará un caso de uso.
func postOn(t *testing.T, tx postgres.Tx, entity, cash, deposit ids.ID, day calendar.Date) (ids.ID, error) {
	t.Helper()
	cop, err := money.CurrencyOf("COP")
	if err != nil {
		return ids.ID{}, err
	}
	posting, err := ledger.New(cal).Post(t.Context(), tx, ledger.PostInput{
		LegalEntityID: entity, Code: "deposito", ValueDate: dateOf(day), BusinessDate: dateOf(day),
		Source: "test", ExternalRef: ids.New().String(), Actor: "tester", Channel: "test", RuleVersion: "v1",
		Entries: []ledger.EntryInput{{AccountID: cash, Amount: money.New(100, cop)}, {AccountID: deposit, Amount: money.New(-100, cop)}},
	})
	return posting.ID, err
}

func ledgerAccounts(t *testing.T, entity ids.ID) (cash, deposit ids.ID) {
	t.Helper()
	cop, err := money.CurrencyOf("COP")
	if err != nil {
		t.Fatal(err)
	}
	err = postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		for _, a := range []struct {
			id   *ids.ID
			kind ledger.Kind
		}{{&cash, ledger.KindAsset}, {&deposit, ledger.KindLiability}} {
			created, err := ledger.New(cal).CreateAccount(t.Context(), tx, ledger.CreateAccountInput{
				LegalEntityID: entity, Code: "cta-" + ids.New().String(), Kind: a.kind, Currency: cop, Mode: ledger.BalanceModeSync,
			})
			if err != nil {
				return err
			}
			*a.id = created.ID
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return cash, deposit
}
