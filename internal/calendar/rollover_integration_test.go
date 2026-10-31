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
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

var (
	testPool *pgxpool.Pool
	cal      = calendar.New()
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, &testPool)) }

// newEntity registers a legal entity with its business date. Whoever operates the deployment creates
// them, not the application (aluna_app has no INSERT): the test switches to the tables' owner.
func newEntity(t *testing.T, businessDate calendar.Date) ids.ID {
	t.Helper()
	id := ids.New()
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO calendar.legal_entities (id, name, timezone, business_date, functional_currency)
			VALUES ($1, 'Test entity', 'America/Bogota', $2, 'COP')`, id, dateOf(businessDate))
		return err
	})
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

// TestRolloverAdvancesToTheNextBusinessDay: from Friday June 27, 2025 it moves to Tuesday July 1
// (Saturday, Sunday and Monday the 30th, a holiday, are not business days).
func TestRolloverAdvancesToTheNextBusinessDay(t *testing.T) {
	friday := calendar.Date{Year: 2025, Month: time.June, Day: 27}
	entity := newEntity(t, friday)

	next, err := rollover(t, entity, friday)

	want := calendar.Date{Year: 2025, Month: time.July, Day: 1}
	if err != nil || next != want {
		t.Fatalf("Rollover = %v, %v; want %v", next, err, want)
	}
	if got := businessDate(t, entity); got != want {
		t.Fatalf("business date = %v, want %v", got, want)
	}
}

// TestARepeatedRolloverIsStale: the second rollover with the same expected date does not advance again.
func TestARepeatedRolloverIsStale(t *testing.T) {
	monday := calendar.Date{Year: 2025, Month: time.July, Day: 7}
	entity := newEntity(t, monday)
	if _, err := rollover(t, entity, monday); err != nil {
		t.Fatal(err)
	}

	if _, err := rollover(t, entity, monday); !errors.Is(err, calendar.ErrStaleBusinessDate) {
		t.Fatalf("second rollover = %v, want ErrStaleBusinessDate", err)
	}
	if got, want := businessDate(t, entity), (calendar.Date{Year: 2025, Month: time.July, Day: 8}); got != want {
		t.Fatalf("business date = %v, want %v: it advanced twice", got, want)
	}
}

func TestAnUnknownEntityHasNoBusinessDate(t *testing.T) {
	unknown := ids.New()
	if _, err := cal.BusinessDate(t.Context(), testPool, unknown); !errors.Is(err, calendar.ErrLegalEntityNotFound) {
		t.Errorf("BusinessDate = %v, want ErrLegalEntityNotFound", err)
	}
	if _, err := rollover(t, unknown, calendar.Date{Year: 2025, Month: time.July, Day: 7}); !errors.Is(err, calendar.ErrLegalEntityNotFound) {
		t.Errorf("Rollover = %v, want ErrLegalEntityNotFound", err)
	}
}

// TestTheBusinessDateOnlyMovesForward: even with direct SQL, the application role cannot move the
// business date back, leave it unchanged or set it to 'infinity', cannot create or delete entities and
// cannot change anything but the date.
func TestTheBusinessDateOnlyMovesForward(t *testing.T) {
	entity := newEntity(t, calendar.Date{Year: 2025, Month: time.July, Day: 7})
	cases := map[string]struct{ sql, sqlState string }{
		"move back":             {`UPDATE calendar.legal_entities SET business_date = '2025-07-04' WHERE id = $1`, "2F003"},
		"leave unchanged":       {`UPDATE calendar.legal_entities SET business_date = business_date WHERE id = $1`, "2F003"},
		"set to infinity":       {`UPDATE calendar.legal_entities SET business_date = 'infinity' WHERE id = $1`, "23514"},
		"change the currency":   {`UPDATE calendar.legal_entities SET functional_currency = 'USD' WHERE id = $1`, "42501"},
		"delete":                {`DELETE FROM calendar.legal_entities WHERE id = $1`, "42501"},
		"create another entity": {`INSERT INTO calendar.legal_entities (id, name, timezone, business_date, functional_currency) SELECT gen_random_uuid(), name, timezone, business_date, functional_currency FROM calendar.legal_entities WHERE id = $1`, "42501"},
	}
	for name, tc := range cases {
		_, err := testPool.Exec(t.Context(), tc.sql, entity)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != tc.sqlState {
			t.Errorf("%s = %v, want SQLSTATE %s", name, err, tc.sqlState)
		}
	}
}

// TestAPostingKeepsTheDateItRead: a transaction reads Monday and stays open; the rollover to Tuesday
// does not wait for it (2 s lock_timeout: if the read locked the row, it would fail), and the
// transaction then posts with the Monday it read and commits.
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
		t.Fatalf("the transaction read %v", d)
	}

	if _, err := rollover(t, entity, monday); err != nil {
		t.Fatalf("the rollover waited for the open transaction: %v", err)
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	stored, err := testLedger().Transaction(t.Context(), testPool, entity, posted)
	if err != nil || !stored.BusinessDate.Equal(dateOf(monday)) {
		t.Fatalf("the posting got %v (%v), want the Monday it read", stored.BusinessDate, err)
	}
	if got := businessDate(t, entity); got != tuesday {
		t.Fatalf("business date = %v, want %v", got, tuesday)
	}
}

// TestRolloverDuringPostings (QS-2): 50 transactions read the business date and post with it while the
// date rolls over, and none fails. Some read Monday and post after the rollover commits (as many as the
// pool's connections minus one: each holds its own and the rollover needs another), the rest up to half
// run alongside the rollover and the other half start afterwards; each transaction keeps the date it
// read.
// They post through the ledger, the module that reads the date in production: the test covers the
// contract between the two modules.
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
	held.Wait() // they already read Monday and are waiting
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
		t.Fatalf("failed postings: %v", err)
	}
	checkPostedOnTheDateRead(t, entity, posted, map[calendar.Date]int{monday: holders, tuesday: 20})
}

type postedOn struct {
	tx   ids.ID
	read calendar.Date
}

// postAfter reads the business date when the transaction starts, signals on read that it did, waits for
// wait and posts with it.
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

// checkPostedOnTheDateRead: each transaction kept the date it read, and at least atLeast[d] were posted
// with each date d.
func checkPostedOnTheDateRead(t *testing.T, entity ids.ID, posted []postedOn, atLeast map[calendar.Date]int) {
	t.Helper()
	seen := map[calendar.Date]int{}
	for _, p := range posted {
		stored, err := testLedger().Transaction(t.Context(), testPool, entity, p.tx)
		if err != nil || !stored.BusinessDate.Equal(dateOf(p.read)) {
			t.Fatalf("transaction %s: business date %v, it had read %v (%v)", p.tx, stored.BusinessDate, p.read, err)
		}
		seen[p.read]++
	}
	for d, n := range atLeast {
		if seen[d] < n {
			t.Fatalf("%d transactions on %v, want at least %d (%v)", seen[d], d, n, seen)
		}
	}
}

// testLedger is the ledger over the tests' calendar, with its events going to the outbox; it is created
// on first use, once the database exists.
var testLedger = sync.OnceValue(func() *ledger.Service {
	events, err := outbox.New(testPool)
	if err != nil {
		panic(err)
	}
	return ledger.New(cal, events)
})

// postOn posts, within tx, a deposit with business date day, as a use case will.
func postOn(t *testing.T, tx postgres.Tx, entity, cash, deposit ids.ID, day calendar.Date) (ids.ID, error) {
	t.Helper()
	cop, err := money.CurrencyOf("COP")
	if err != nil {
		return ids.ID{}, err
	}
	posting, err := testLedger().Post(t.Context(), tx, ledger.PostInput{
		LegalEntityID: entity, Code: "deposit", ValueDate: dateOf(day), BusinessDate: dateOf(day),
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
			created, err := testLedger().CreateAccount(t.Context(), tx, ledger.CreateAccountInput{
				LegalEntityID: entity, Code: "acct-" + ids.New().String(), Kind: a.kind, Currency: cop, Mode: ledger.BalanceModeSync,
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
