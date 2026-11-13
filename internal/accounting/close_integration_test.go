//go:build integration

package accounting_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/DrewGGM/aluna/internal/accounting"
	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// closeWorld is a glWorld on business date 2026-03-02 with a posting in February.
type closeWorld struct {
	glWorld
	cash, deposits ids.ID
}

func newCloseWorld(t *testing.T) closeWorld {
	t.Helper()
	w := closeWorld{glWorld: newGLWorld(t)}
	w.cash, w.deposits = w.account(t, "A.CASH.VAULT", ledger.KindAsset), w.account(t, "deposits/1", ledger.KindLiability)
	w.post(t, "2026-02-27", w.cash, w.deposits, 500)
	return w
}

func (w closeWorld) close(t *testing.T, period string) error {
	t.Helper()
	return postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		return w.s.ClosePeriod(t.Context(), tx, w.entity, period, "finance-b", w.anyOpenBreak(tx))
	})
}

// anyOpenBreak answers, within tx, what reconciliation answers in the application: whether the entity has a break
// open.
func (w closeWorld) anyOpenBreak(tx postgres.Tx) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		var open bool
		err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM recon.breaks WHERE legal_entity_id = $1 AND status = 'open')`, w.entity).Scan(&open)
		return open, err
	}
}

// postIn is a posting of the world with business date on, and its error.
func (w closeWorld) postIn(t *testing.T, tx postgres.Tx, on string) error {
	t.Helper()
	date := day(t, on)
	_, err := w.ledger.Post(t.Context(), tx, ledger.PostInput{
		LegalEntityID: w.entity, Code: "test", ValueDate: date, BusinessDate: date, Source: "test", ExternalRef: ids.New().String(),
		Actor: "tester", Channel: "test", RuleVersion: "v1",
		Entries: []ledger.EntryInput{{AccountID: w.cash, Amount: money.New(10, w.cop)}, {AccountID: w.deposits, Amount: money.New(-10, w.cop)}},
	})
	return err
}

// openBreak opens a break of entity.
func openBreak(t *testing.T, entity ids.ID) {
	t.Helper()
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO recon.breaks (id, legal_entity_id, control, subject, currency, difference_units)
			VALUES ($1, $2, 'trial_balance', 'COP', 'COP', 1)`, ids.New(), entity)
		return err
	})
}

// TestAClosedPeriodTakesNoPostings (FR-37, US-14): a square period without breaks closes, by who, once, and from then
// on nothing posts in it; the next month still takes postings, and neither a resolved break nor another entity's
// hold it.
func TestAClosedPeriodTakesNoPostings(t *testing.T) {
	w := newCloseWorld(t)
	openBreak(t, ids.New())
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO recon.breaks (id, legal_entity_id, control, subject, currency, difference_units, status, cause, resolved_by)
			VALUES ($1, $2, 'trial_balance', 'COP', 'COP', 1, 'resolved', 'a late posting', 'finance-a')`, ids.New(), w.entity)
		return err
	})

	if err := w.close(t, "2026-02"); err != nil {
		t.Fatal(err)
	}

	var status, by string
	var closedAt *time.Time
	if err := testPool.QueryRow(t.Context(), `SELECT status, closed_by, closed_at FROM calendar.periods WHERE legal_entity_id = $1 AND period = '2026-02'`,
		w.entity).Scan(&status, &by, &closedAt); err != nil {
		t.Fatal(err)
	}
	if status != "closed" || by != "finance-b" || closedAt == nil {
		t.Fatalf("period %s by %s at %v", status, by, closedAt)
	}
	if err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error { return w.postIn(t, tx, "2026-02-28") }); !errors.Is(err, ledger.ErrPeriodClosed) {
		t.Fatalf("posting in February = %v, want ErrPeriodClosed", err)
	}
	w.post(t, "2026-03-02", w.cash, w.deposits, 10)
	openBreak(t, w.entity)
	if err := w.close(t, "2026-02"); !errors.Is(err, calendar.ErrPeriodClosed) {
		t.Fatalf("closing it again = %v, want ErrPeriodClosed", err)
	}
	if err := w.s.CheckPeriodClose(t.Context(), testPool, w.entity, "2026-02"); !errors.Is(err, calendar.ErrPeriodClosed) {
		t.Fatalf("checking it again = %v, want ErrPeriodClosed", err)
	}
}

func TestAPeriodThatHasNotEndedDoesNotClose(t *testing.T) {
	w := newCloseWorld(t)
	if err := w.close(t, "2026-03"); !errors.Is(err, calendar.ErrPeriodNotEnded) {
		t.Fatalf("closing the business month = %v, want ErrPeriodNotEnded", err)
	}
	if err := w.close(t, "2026-2"); !errors.Is(err, calendar.ErrInvalidPeriod) {
		t.Fatalf("closing 2026-2 = %v, want ErrInvalidPeriod", err)
	}
}

// TestABreakOpenKeepsThePeriodOpen: the close fails and undoes the closing.
func TestABreakOpenKeepsThePeriodOpen(t *testing.T) {
	w := newCloseWorld(t)
	openBreak(t, w.entity)

	if err := w.close(t, "2026-02"); !errors.Is(err, accounting.ErrOpenBreaks) {
		t.Fatalf("close = %v, want ErrOpenBreaks", err)
	}
	if err := w.s.CheckPeriodClose(t.Context(), testPool, w.entity, "2026-02"); err != nil {
		t.Fatalf("the period after a failed close: %v", err)
	}
	w.post(t, "2026-02-28", w.cash, w.deposits, 10)
}

// TestAPeriodThatDoesNotSquareDoesNotClose: the trial balance is the period's own, from its first day to its last. The
// database refuses an unbalanced transaction when it commits, so each leg only lives inside the close's transaction.
func TestAPeriodThatDoesNotSquareDoesNotClose(t *testing.T) {
	w := newCloseWorld(t)
	errUndo := errors.New("undo")
	for _, c := range []struct {
		legs []string
		want error
	}{
		{[]string{"2026-02-01"}, accounting.ErrPeriodNotSquare},
		{[]string{"2026-02-28"}, accounting.ErrPeriodNotSquare},
		{[]string{"2026-01-31", "2026-03-01"}, errUndo},
	} {
		err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
			for _, on := range c.legs {
				w.oneLeg(t, tx, on)
			}
			if err := w.s.ClosePeriod(t.Context(), tx, w.entity, "2026-02", "finance-b", w.anyOpenBreak(tx)); err != nil {
				return err
			}
			return errUndo
		})
		if !errors.Is(err, c.want) {
			t.Fatalf("legs on %v: close = %v, want %v", c.legs, err, c.want)
		}
		if errors.Is(c.want, accounting.ErrPeriodNotSquare) && !strings.Contains(err.Error(), "COP") {
			t.Errorf("close = %v, want the currency", err)
		}
	}
}

// oneLeg writes a transaction with a single entry, with business date on, within tx.
func (w closeWorld) oneLeg(t *testing.T, tx postgres.Tx, on string) {
	t.Helper()
	id := ids.New()
	if _, err := tx.Exec(t.Context(), `INSERT INTO ledger.transactions
		(id, legal_entity_id, code, value_date, business_date, source, external_ref, actor, channel, rule_version)
		VALUES ($1, $2, 'test', $3, $3, 'test', $4, 'tester', 'test', 'v1')`, id, w.entity, on, id.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO ledger.entries (transaction_id, legal_entity_id, account_id, currency, amount, value_date, posted_at)
		VALUES ($1, $2, $3, 'COP', 7, $4, clock_timestamp())`, id, w.entity, w.cash, on); err != nil {
		t.Fatal(err)
	}
}

// TestTheCloseWaitsForAPostingInFlight: a posting that checked the period before the close and has not committed is
// in the trial balance the close reads, because the close waits for it.
func TestTheCloseWaitsForAPostingInFlight(t *testing.T) {
	w := newCloseWorld(t)
	posted, release, posting := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		posting <- postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
			if err := w.postIn(t, tx, "2026-02-28"); err != nil {
				return err
			}
			close(posted)
			<-release
			return nil
		})
	}()
	<-posted
	closing := make(chan error, 1)
	go func() { closing <- w.close(t, "2026-02") }()

	waitsForThePeriod(t, w.entity, "2026-02")
	close(release)

	if err := <-posting; err != nil {
		t.Fatal(err)
	}
	if err := <-closing; err != nil {
		t.Fatal(err)
	}
}

// TestAPostingThatCheckedBeforeTheCloseIsRefusedAfterIt: one that saw the period open and writes after the close
// committed is refused by the database. The ledger still hands over its error as it is, not as ErrPeriodClosed
// (open question Q-131).
func TestAPostingThatCheckedBeforeTheCloseIsRefusedAfterIt(t *testing.T) {
	w := newCloseWorld(t)
	posting := make(chan error, 1)
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `SELECT pg_advisory_xact_lock(calendar.period_lock_key($1, '2026-02'))`, w.entity); err != nil {
			return err
		}
		go func() {
			posting <- postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error { return w.postIn(t, tx, "2026-02-28") })
		}()
		waitsForThePeriod(t, w.entity, "2026-02")
		_, err := tx.Exec(t.Context(), `INSERT INTO calendar.periods (legal_entity_id, period, status, closed_at, closed_by)
			VALUES ($1, '2026-02', 'closed', clock_timestamp(), 'finance-b')`, w.entity)
		return err
	})

	var pgErr *pgconn.PgError
	if err := <-posting; !errors.As(err, &pgErr) || pgErr.ConstraintName != "transactions_in_open_period" {
		t.Fatalf("posting = %v, want refused by transactions_in_open_period", err)
	}
}

// waitsForThePeriod returns once someone waits for the advisory lock of the entity's period.
func waitsForThePeriod(t *testing.T, entity ids.ID, period string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		var waiting bool
		if err := testPool.QueryRow(t.Context(), `WITH k AS (SELECT calendar.period_lock_key($1, $2) AS key)
			SELECT EXISTS (SELECT 1 FROM pg_locks, k WHERE locktype = 'advisory' AND NOT granted AND objsubid = 1
				AND classid::bigint = (key >> 32) & 4294967295 AND objid::bigint = key & 4294967295)`, entity, period).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
	}
	t.Fatalf("nobody waits for the lock of %s", period)
}
