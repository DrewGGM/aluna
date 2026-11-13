//go:build integration

package accounting_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/accounting"
	pg "github.com/DrewGGM/aluna/internal/accounting/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// glWorld is an entity on business date 2026-03-02 with a ledger and mappings for its cash and its customers.
type glWorld struct {
	entity ids.ID
	cop    money.Currency
	ledger *ledger.Service
	s      *accounting.Service
}

func newGLWorld(t *testing.T) glWorld {
	t.Helper()
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	w := glWorld{entity: ids.New()}
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO calendar.legal_entities (id, name, business_date, functional_currency)
			VALUES ($1, 'Entity', '2026-03-02', 'COP')`, w.entity)
		return err
	})
	if w.cop, err = money.CurrencyOf("COP"); err != nil {
		t.Fatal(err)
	}
	cal := calendar.New(events, time.Now)
	w.ledger, w.s = ledger.New(cal, events), accounting.New(cal, events)
	if err := load(t, w.s, w.entity, mappings(t, "A.CASH.,110505,2026-03-02,2030-01-01\ndeposits/,210505,2026-03-02,2030-01-01\n")); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w glWorld) account(t *testing.T, code string, kind ledger.Kind) ids.ID {
	t.Helper()
	var a ledger.Account
	inTx(t, func(tx postgres.Tx) error {
		var err error
		a, err = w.ledger.CreateAccount(t.Context(), tx, ledger.CreateAccountInput{
			LegalEntityID: w.entity, Code: code, Kind: kind, Currency: w.cop, Mode: ledger.BalanceModeSync, LowerLimit: -1_000_000_000,
		})
		return err
	})
	return a.ID
}

// post moves units from credit to debit with business date on.
func (w glWorld) post(t *testing.T, on string, debit, credit ids.ID, units int64) {
	t.Helper()
	date := day(t, on)
	inTx(t, func(tx postgres.Tx) error {
		_, err := w.ledger.Post(t.Context(), tx, ledger.PostInput{
			LegalEntityID: w.entity, Code: "test", ValueDate: date, BusinessDate: date, Source: "test", ExternalRef: ids.New().String(),
			Actor: "tester", Channel: "test", RuleVersion: "v1",
			Entries: []ledger.EntryInput{{AccountID: debit, Amount: money.New(units, w.cop)}, {AccountID: credit, Amount: money.New(-units, w.cop)}},
		})
		return err
	})
}

// rollover moves the entity's business date to day, closing the days before.
func (w glWorld) rollover(t *testing.T, day string) {
	t.Helper()
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE calendar.legal_entities SET business_date = $2 WHERE id = $1`, w.entity, day)
		return err
	})
}

func (w glWorld) project(t *testing.T) {
	t.Helper()
	if err := w.s.ProjectGL(t.Context(), postgres.NewUnitOfWork(testPool)); err != nil {
		t.Fatal(err)
	}
}

func inTx(t *testing.T, fn func(tx postgres.Tx) error) {
	t.Helper()
	if err := postgres.NewUnitOfWork(testPool).Do(t.Context(), fn); err != nil {
		t.Fatal(err)
	}
}

// glLine is a GL line as these tests compare it.
type glLine struct {
	code            string
	debits, credits int64
}

func (w glWorld) gl(t *testing.T, day string) ([]glLine, error) {
	t.Helper()
	gl, err := w.s.GL(t.Context(), testPool, w.entity, on(t, day))
	if err != nil {
		return nil, err
	}
	out := make([]glLine, len(gl.Lines))
	var debits, credits int64
	for i, l := range gl.Lines {
		out[i] = glLine{l.CUIFCode, l.Debits.Units(), l.Credits.Units()}
		debits, credits = debits+l.Debits.Units(), credits+l.Credits.Units()
	}
	if debits != credits || gl.ProjectedAt.IsZero() {
		t.Fatalf("GL of %s = %+v: debits %d, credits %d", day, gl, debits, credits)
	}
	return out, nil
}

// TestAClosedDayIsProjectedByCUIFCode (FR-36, US-13): the customers' accounts add up into their code, the day sums
// zero, and the open day has no projection yet.
func TestAClosedDayIsProjectedByCUIFCode(t *testing.T) {
	w := newGLWorld(t)
	vault := w.account(t, "A.CASH.VAULT", ledger.KindAsset)
	a, b := w.account(t, "deposits/"+ids.New().String(), ledger.KindLiability), w.account(t, "deposits/"+ids.New().String(), ledger.KindLiability)
	w.post(t, "2026-03-02", vault, a, 100_00)
	w.post(t, "2026-03-02", vault, b, 30_00)
	w.post(t, "2026-03-02", a, vault, 10_00)

	w.project(t)
	if _, err := w.gl(t, "2026-03-02"); !errors.Is(err, accounting.ErrNotProjected) {
		t.Fatalf("the open day = %v, want ErrNotProjected", err)
	}

	w.rollover(t, "2026-03-03")
	w.project(t)

	got, err := w.gl(t, "2026-03-02")
	if err != nil {
		t.Fatal(err)
	}
	if want := []glLine{{"110505", 130_00, 10_00}, {"210505", 10_00, 130_00}}; !slices.Equal(got, want) {
		t.Fatalf("GL = %v, want %v", got, want)
	}
}

// TestALateTransactionIsProjectedAgain (QS-2): one that read the closed day commits after its projection; the next
// run projects the day again, from the journal.
func TestALateTransactionIsProjectedAgain(t *testing.T) {
	w := newGLWorld(t)
	vault, customer := w.account(t, "A.CASH.VAULT", ledger.KindAsset), w.account(t, "deposits/"+ids.New().String(), ledger.KindLiability)
	w.post(t, "2026-03-02", vault, customer, 100_00)
	w.rollover(t, "2026-03-03")
	w.project(t)

	first, err := w.s.GL(t.Context(), testPool, w.entity, on(t, "2026-03-02"))
	if err != nil {
		t.Fatal(err)
	}

	w.post(t, "2026-03-02", vault, customer, 5_00)
	w.project(t)

	got, err := w.gl(t, "2026-03-02")
	if err != nil {
		t.Fatal(err)
	}
	if want := []glLine{{"110505", 105_00, 0}, {"210505", 0, 105_00}}; !slices.Equal(got, want) {
		t.Fatalf("GL = %v, want %v", got, want)
	}
	if again, err := w.s.GL(t.Context(), testPool, w.entity, on(t, "2026-03-02")); err != nil || !again.ProjectedAt.After(first.ProjectedAt) {
		t.Fatalf("projected at %v, then %v (%v): want it recorded again", first.ProjectedAt, again.ProjectedAt, err)
	}
	w.settles(t, "2026-03-02")
}

// TestATransactionInProgressDuringTheProjectionIsProjectedLater (QS-2): it read the day before the rollover and
// commits after the projection, which could not see it; the next run does.
func TestATransactionInProgressDuringTheProjectionIsProjectedLater(t *testing.T) {
	w := newGLWorld(t)
	vault, customer := w.account(t, "A.CASH.VAULT", ledger.KindAsset), w.account(t, "deposits/"+ids.New().String(), ledger.KindLiability)
	w.post(t, "2026-03-02", vault, customer, 100_00)
	posted, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
			_, err := w.ledger.Post(t.Context(), tx, ledger.PostInput{
				LegalEntityID: w.entity, Code: "test", ValueDate: day(t, "2026-03-02"), BusinessDate: day(t, "2026-03-02"), Source: "test",
				ExternalRef: ids.New().String(), Actor: "tester", Channel: "test", RuleVersion: "v1",
				Entries: []ledger.EntryInput{{AccountID: vault, Amount: money.New(7_00, w.cop)}, {AccountID: customer, Amount: money.New(-7_00, w.cop)}},
			})
			close(posted)
			<-release
			return err
		})
	}()
	<-posted
	w.rollover(t, "2026-03-03")
	w.project(t)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	w.project(t)

	got, err := w.gl(t, "2026-03-02")
	if err != nil {
		t.Fatal(err)
	}
	if want := []glLine{{"110505", 107_00, 0}, {"210505", 0, 107_00}}; !slices.Equal(got, want) {
		t.Fatalf("GL = %v, want %v", got, want)
	}
}

func day(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestADayWithAnUnmappedAccountHasNoGL: it is recorded unmapped and without lines, the other days are projected, and
// it is not projected again while no transaction reaches it.
func TestADayWithAnUnmappedAccountHasNoGL(t *testing.T) {
	w := newGLWorld(t)
	vault, customer := w.account(t, "A.CASH.VAULT", ledger.KindAsset), w.account(t, "deposits/"+ids.New().String(), ledger.KindLiability)
	other := w.account(t, "L.OTHER", ledger.KindLiability)
	w.post(t, "2026-03-02", vault, other, 1_00)
	w.post(t, "2026-03-03", vault, customer, 2_00)
	w.rollover(t, "2026-03-04")

	w.project(t)

	if _, err := w.gl(t, "2026-03-02"); !errors.Is(err, accounting.ErrDayUnmapped) {
		t.Fatalf("the unmapped day = %v, want ErrDayUnmapped", err)
	}
	if got, err := w.gl(t, "2026-03-03"); err != nil || len(got) != 2 {
		t.Fatalf("the mapped day = %v, %v", got, err)
	}
	w.settles(t, "2026-03-02")
}

// settles projects until day is no longer pending: it is not projected again with nothing new. A transaction of
// another test of the cluster that was open at the projection keeps it pending until it ends, so it tries for a
// while.
func (w glWorld) settles(t *testing.T, day string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for slices.Contains(w.pending(t), day) {
		if time.Now().After(deadline) {
			t.Fatalf("%s is still pending: it is projected again with nothing new", day)
		}
		w.project(t)
		time.Sleep(50 * time.Millisecond)
	}
}

// TestTheNewestDaysGoFirst: a catch-up projects the recent days before the old ones.
func TestTheNewestDaysGoFirst(t *testing.T) {
	w := newGLWorld(t)
	vault, customer := w.account(t, "A.CASH.VAULT", ledger.KindAsset), w.account(t, "deposits/"+ids.New().String(), ledger.KindLiability)
	w.post(t, "2026-03-02", vault, customer, 1_00)
	w.post(t, "2026-03-03", vault, customer, 1_00)
	w.rollover(t, "2026-03-04")

	if got, want := w.pending(t), []string{"2026-03-03", "2026-03-02"}; !slices.Equal(got, want) {
		t.Fatalf("days to project = %v, want %v", got, want)
	}
}

// TestALateTransactionToAnUnmappedAccountTakesTheGLAway: a day already projected that an unmapped account reaches
// afterwards is not served incomplete.
func TestALateTransactionToAnUnmappedAccountTakesTheGLAway(t *testing.T) {
	w := newGLWorld(t)
	vault, customer := w.account(t, "A.CASH.VAULT", ledger.KindAsset), w.account(t, "deposits/"+ids.New().String(), ledger.KindLiability)
	w.post(t, "2026-03-02", vault, customer, 100_00)
	w.rollover(t, "2026-03-03")
	w.project(t)

	w.post(t, "2026-03-02", vault, w.account(t, "L.OTHER", ledger.KindLiability), 5_00)
	w.project(t)

	if _, err := w.gl(t, "2026-03-02"); !errors.Is(err, accounting.ErrDayUnmapped) {
		t.Fatalf("GL = %v, want ErrDayUnmapped", err)
	}
}

// TestTheDaysBeforeTheFirstMappingHaveNoGL: an entity that starts mapping with history, or does not map at all,
// leaves nothing to project before.
func TestTheDaysBeforeTheFirstMappingHaveNoGL(t *testing.T) {
	w := newGLWorld(t)
	vault, customer := w.account(t, "A.CASH.VAULT", ledger.KindAsset), w.account(t, "deposits/"+ids.New().String(), ledger.KindLiability)
	w.post(t, "2026-03-01", vault, customer, 1_00)
	w.rollover(t, "2026-03-03")

	w.project(t)

	if _, err := w.gl(t, "2026-03-01"); !errors.Is(err, accounting.ErrNotProjected) {
		t.Fatalf("a day before the first mapping = %v, want ErrNotProjected", err)
	}
	if pending := w.pending(t); len(pending) != 0 {
		t.Fatalf("days to project = %v, want none", pending)
	}
}

// pending are the world's days that a projection would take.
func (w glWorld) pending(t *testing.T) []string {
	t.Helper()
	var days []string
	inTx(t, func(tx postgres.Tx) error {
		all, err := pg.NewStore(tx).DaysToProject(t.Context())
		for _, d := range all {
			if d.LegalEntityID == w.entity {
				days = append(days, d.Date.String())
			}
		}
		return err
	})
	return days
}

// TestAProjectionIsRebuiltNotEdited: projecting again gives the same lines, and a line of the day that is no longer
// in the journal goes away.
func TestAProjectionIsRebuiltNotEdited(t *testing.T) {
	w := newGLWorld(t)
	vault, customer := w.account(t, "A.CASH.VAULT", ledger.KindAsset), w.account(t, "deposits/"+ids.New().String(), ledger.KindLiability)
	w.post(t, "2026-03-02", vault, customer, 100_00)
	w.rollover(t, "2026-03-03")
	w.project(t)
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO accounting.gl_lines (legal_entity_id, business_date, cuif_code, currency, debit_units, credit_units)
			VALUES ($1, '2026-03-02', '999999', 'COP', 1, 1)`, w.entity)
		if err != nil {
			return err
		}
		_, err = tx.Exec(t.Context(), `UPDATE accounting.gl_days SET seen_below = '1'::xid8 WHERE legal_entity_id = $1`, w.entity)
		return err
	})

	w.project(t)

	got, err := w.gl(t, "2026-03-02")
	if err != nil {
		t.Fatal(err)
	}
	if want := []glLine{{"110505", 100_00, 0}, {"210505", 0, 100_00}}; !slices.Equal(got, want) {
		t.Fatalf("GL = %v, want %v", got, want)
	}
}

// TestAnotherEntitysDayIsItsOwn: the GL reads the entity's projection only.
func TestAnotherEntitysDayIsItsOwn(t *testing.T) {
	w, other := newGLWorld(t), newGLWorld(t)
	vault, customer := w.account(t, "A.CASH.VAULT", ledger.KindAsset), w.account(t, "deposits/"+ids.New().String(), ledger.KindLiability)
	w.post(t, "2026-03-02", vault, customer, 1_00)
	w.rollover(t, "2026-03-03")
	w.project(t)

	if _, err := other.gl(t, "2026-03-02"); !errors.Is(err, accounting.ErrNotProjected) {
		t.Fatalf("another entity's GL = %v, want ErrNotProjected", err)
	}
}
