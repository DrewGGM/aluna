//go:build integration

package reconciliation_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
	"github.com/DrewGGM/aluna/internal/reconciliation"
)

const typeBreakResolved = "aluna.reconciliation.break.resolved.v1"

// inTheMorning is an instant of 2026-03-02 in America/Bogota.
var inTheMorning = time.Date(2026, 3, 2, 14, 0, 0, 0, time.UTC)

// tag makes name an end-to-end id of the world alone: the database keeps them unique across entities.
func (w world) tag(name string) string { return name + "-" + w.entity.String()[28:] }

// order stores a payment order of the world's entity on the simulated rail.
func (w world) order(t *testing.T, direction, e2e string, units int64, status string, at time.Time) {
	t.Helper()
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		var account, hold, agent any
		if direction == "outgoing" {
			account, hold = ids.New(), ids.New()
		} else {
			agent = "BANK"
		}
		id := ids.New().String()[:35]
		_, err := tx.Exec(t.Context(), `INSERT INTO payments.orders (id, legal_entity_id, account_id, direction, rail, end_to_end_id, instr_id,
				tx_id, currency, amount_units, creditor_number, status, external_ref, hold_id, debtor_agent, created_at)
			VALUES ($1, $2, $3, $4, 'simbreb', $5, 'INSTR', 'TX', 'COP', $6, '123', $7, $8, $9, $10, $11)`,
			ids.New(), w.entity, account, direction, e2e, units, status, id, hold, agent, at)
		return err
	})
}

func (w world) line(e2e string, direction reconciliation.Direction, units int64) reconciliation.StatementLine {
	return reconciliation.StatementLine{EndToEndID: e2e, Direction: direction, Amount: money.New(units, w.cop), BookingDate: statementDate}
}

var statementDate = time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)

func (w world) statement(lines ...reconciliation.StatementLine) reconciliation.Statement {
	return reconciliation.Statement{
		LegalEntityID: w.entity, Rail: "simbreb", Ref: "STMT-1", Date: statementDate, ImportedBy: "finance-a",
		Lines: append([]reconciliation.StatementLine{}, lines...),
	}
}

func (w world) importing(t *testing.T, s reconciliation.Statement) (reconciliation.ImportResult, error) {
	t.Helper()
	var got reconciliation.ImportResult
	err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		var err error
		got, err = w.recon.ImportStatement(t.Context(), tx, s)
		return err
	})
	return got, err
}

func (w world) imported(t *testing.T, s reconciliation.Statement) reconciliation.ImportResult {
	t.Helper()
	got, err := w.importing(t, s)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// statementBreak is what a statement's break stores.
type statementBreak struct {
	subject, reason, owner string
	difference             int64
	ofAStatement           bool
}

func (w world) statementBreaks(t *testing.T) []statementBreak {
	t.Helper()
	rows, err := testPool.Query(t.Context(), `SELECT subject, reason, owner, difference_units, statement_id IS NOT NULL FROM recon.breaks
		WHERE legal_entity_id = $1 AND control = 'statement_match' ORDER BY subject`, w.entity)
	if err != nil {
		t.Fatal(err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (statementBreak, error) {
		var b statementBreak
		err := row.Scan(&b.subject, &b.reason, &b.owner, &b.difference, &b.ofAStatement)
		return b, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// TestALineInAnotherAmountOpensAnException (US-9, FR-34): the statement is stored, the line that matches passes
// and the other one opens a break with its owner, and an event.
func TestALineInAnotherAmountOpensAnException(t *testing.T) {
	w := newWorld(t)
	w.order(t, "outgoing", w.tag("GOOD"), 100_00, "settled", inTheMorning)
	w.order(t, "outgoing", w.tag("BAD"), 100_00, "settled", inTheMorning)

	got := w.imported(t, w.statement(w.line(w.tag("GOOD"), reconciliation.Outgoing, 100_00), w.line(w.tag("BAD"), reconciliation.Outgoing, 130_00)))

	if want := (reconciliation.ImportResult{ID: got.ID, Lines: 2, Matched: 1, Exceptions: 1}); got != want {
		t.Errorf("result = %+v, want %+v", got, want)
	}
	found := w.statementBreaks(t)
	if len(found) != 1 || found[0] != (statementBreak{"outgoing/" + w.tag("BAD"), "amount_mismatch", "finance-a", 30_00, true}) {
		t.Fatalf("breaks = %+v", found)
	}
	if len(w.events.events) != 1 || w.events.events[0].Type != typeBreakOpened || !strings.Contains(fmt.Sprintf("%+v", w.events.events[0].Data), "amount_mismatch") {
		t.Fatalf("events = %+v, want the break opened, with why", w.events.events)
	}
	var lines int
	if err := testPool.QueryRow(t.Context(), `SELECT line_count FROM recon.statements WHERE legal_entity_id = $1 AND statement_ref = 'STMT-1'
		AND statement_date = '2026-03-02' AND imported_by = 'finance-a'`, w.entity).Scan(&lines); err != nil || lines != 2 {
		t.Fatalf("statement with %d lines (%v), want 2", lines, err)
	}
}

// TestBothSidesOfAStatementAreCompared (FR-34): a line nobody knows, a line of an order the core did not settle
// and an order of the day the rail does not report are exceptions; an incoming order that went to suspense came
// in all the same, and an order held or of another day does not concern the day's statement.
func TestBothSidesOfAStatementAreCompared(t *testing.T) {
	w := newWorld(t)
	w.order(t, "outgoing", w.tag("STUCK"), 10_00, "sent", inTheMorning)
	w.order(t, "outgoing", w.tag("MISSING"), 20_00, "settled", inTheMorning)
	w.order(t, "incoming", w.tag("SUSPENDED"), 30_00, "suspended", inTheMorning)
	w.order(t, "outgoing", w.tag("HELD"), 40_00, "held", inTheMorning)
	w.order(t, "outgoing", w.tag("YESTERDAY"), 50_00, "settled", inTheMorning.AddDate(0, 0, -1))
	w.order(t, "outgoing", w.tag("LATE"), 60_00, "settled", inTheMorning.AddDate(0, 0, -1)) // a line of the next day still finds it
	w.order(t, "outgoing", w.tag("NEVER"), 70_00, "sent", inTheMorning)
	w.order(t, "incoming", w.tag("UNREPORTED"), 80_00, "suspended", inTheMorning)
	w.order(t, "incoming", w.tag("OTHERWAY"), 90_00, "settled", inTheMorning.AddDate(0, 0, -1))

	got := w.imported(t, w.statement(
		w.line(w.tag("UNKNOWN"), reconciliation.Incoming, 5_00),
		w.line(w.tag("STUCK"), reconciliation.Outgoing, 10_00),
		w.line(w.tag("SUSPENDED"), reconciliation.Incoming, 30_00),
		w.line(w.tag("LATE"), reconciliation.Outgoing, 60_00),
		w.line(w.tag("OTHERWAY"), reconciliation.Outgoing, 90_00),
	))

	if want := (reconciliation.ImportResult{ID: got.ID, Lines: 5, Matched: 2, Exceptions: 6}); got != want {
		t.Errorf("result = %+v, want %+v", got, want)
	}
	want := []statementBreak{
		{"incoming/" + w.tag("UNKNOWN"), "line_without_order", "finance-a", 5_00, true},
		{"incoming/" + w.tag("UNREPORTED"), "order_without_line", "finance-a", -80_00, true},
		{"outgoing/" + w.tag("MISSING"), "order_without_line", "finance-a", -20_00, true},
		{"outgoing/" + w.tag("NEVER"), "order_without_line", "finance-a", -70_00, true},
		{"outgoing/" + w.tag("OTHERWAY"), "line_without_order", "finance-a", 90_00, true},
		{"outgoing/" + w.tag("STUCK"), "order_not_settled", "finance-a", 10_00, true},
	}
	found := w.statementBreaks(t)
	if len(found) != len(want) {
		t.Fatalf("breaks = %+v, want %+v", found, want)
	}
	for i := range want {
		if found[i] != want[i] {
			t.Errorf("break %d = %+v, want %+v", i, found[i], want[i])
		}
	}
}

// TestTheDayOfAnOrderIsBogotas: at 04:00 UTC of the 3rd it is still the 2nd there, and at 05:00 it is not.
func TestTheDayOfAnOrderIsBogotas(t *testing.T) {
	w := newWorld(t)
	w.order(t, "outgoing", w.tag("LAST-MINUTE"), 10_00, "settled", time.Date(2026, 3, 3, 4, 59, 0, 0, time.UTC))
	w.order(t, "outgoing", w.tag("NEXT-DAY"), 20_00, "settled", time.Date(2026, 3, 3, 5, 0, 0, 0, time.UTC))

	w.imported(t, w.statement())

	found := w.statementBreaks(t)
	if len(found) != 1 || found[0].subject != "outgoing/"+w.tag("LAST-MINUTE") {
		t.Fatalf("breaks = %+v, want the order of the 2nd", found)
	}
}

// TestAStatementGoesInOnce: by entity, rail and reference; the rollback leaves no break of the second try.
func TestAStatementGoesInOnce(t *testing.T) {
	w, other := newWorld(t), newWorld(t)
	w.imported(t, w.statement())

	if _, err := w.importing(t, w.statement(w.line(w.tag("UNKNOWN"), reconciliation.Outgoing, 1_00))); !errors.Is(err, reconciliation.ErrDuplicateStatement) {
		t.Fatalf("second import = %v, want ErrDuplicateStatement", err)
	}
	if found := w.statementBreaks(t); len(found) != 0 {
		t.Errorf("breaks = %+v, want none", found)
	}
	other.imported(t, other.statement())
}

// TestAnotherEntitysOrderIsNotAMatch: an order is the entity's, so the line is unknown here and the order is no
// exception there.
func TestAnotherEntitysOrderIsNotAMatch(t *testing.T) {
	w, other := newWorld(t), newWorld(t)
	theirs := other.tag("THEIRS")
	other.order(t, "outgoing", theirs, 10_00, "settled", inTheMorning)

	got := w.imported(t, w.statement(w.line(theirs, reconciliation.Outgoing, 10_00)))

	if got.Matched != 0 || got.Exceptions != 1 {
		t.Fatalf("result = %+v", got)
	}
}

// resolve resolves break id of the entity as finance-b.
func (w world) resolve(t *testing.T, entity, id ids.ID, cause string) error {
	t.Helper()
	return postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		_, err := w.recon.ResolveBreak(t.Context(), tx, entity, id, cause, "finance-b")
		return err
	})
}

func (w world) page(t *testing.T, status reconciliation.BreakStatus, cursor string, limit int) reconciliation.BreakPage {
	t.Helper()
	page, err := w.recon.Breaks(t.Context(), testPool, w.entity, status, cursor, limit)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

// TestABreakIsResolvedWithACause (FR-34): the database stamps when, the event goes out without the free text, and
// it is resolved once, by its entity.
func TestABreakIsResolvedWithACause(t *testing.T) {
	w, other := newWorld(t), newWorld(t)
	w.imported(t, w.statement(w.line(w.tag("UNKNOWN"), reconciliation.Outgoing, 1_00)))
	opened := w.page(t, reconciliation.BreakOpen, "", 10).Breaks[0]

	if err := w.resolve(t, other.entity, opened.ID, "not mine"); !errors.Is(err, reconciliation.ErrBreakNotFound) {
		t.Fatalf("from another entity: %v, want ErrBreakNotFound", err)
	}
	if err := w.resolve(t, w.entity, opened.ID, "  "); !errors.Is(err, reconciliation.ErrInvalidResolution) {
		t.Fatalf("without a cause: %v, want ErrInvalidResolution", err)
	}
	var resolved reconciliation.Break
	err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		var err error
		resolved, err = w.recon.ResolveBreak(t.Context(), tx, w.entity, opened.ID, "the rail reversed it", "finance-b")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.Resolution.At.Equal(w.page(t, reconciliation.BreakResolved, "", 1).Breaks[0].Resolution.At) {
		t.Fatalf("resolved at %v, not when the database says", resolved.Resolution.At)
	}
	if err := w.resolve(t, w.entity, opened.ID, "again"); !errors.Is(err, reconciliation.ErrBreakResolved) {
		t.Fatalf("twice: %v, want ErrBreakResolved", err)
	}
	w.expectResolved(t)
}

// expectResolved checks the only resolved break of the world, and that its event says nothing of the cause.
func (w world) expectResolved(t *testing.T) {
	t.Helper()
	read := w.page(t, reconciliation.BreakResolved, "", 10).Breaks
	if len(read) != 1 || read[0].Status != reconciliation.BreakResolved || read[0].Resolution == nil {
		t.Fatalf("resolved = %+v", read)
	}
	if r := read[0].Resolution; r.Cause != "the rail reversed it" || r.By != "finance-b" || !r.At.After(read[0].OpenedAt) {
		t.Fatalf("resolution = %+v", r)
	}
	if last := w.events.events[len(w.events.events)-1]; last.Type != typeBreakResolved || strings.Contains(fmt.Sprint(last.Data), "reversed") {
		t.Errorf("event = %+v", last)
	}
}

// TestADifferenceThatStaysOpensAgain: resolving a break does not fix the ledger, and the next run says so.
func TestADifferenceThatStaysOpensAgain(t *testing.T) {
	w := newWorld(t)
	vault, other := w.account(t, "A.CASH.VAULT", ledger.KindAsset, ledger.BalanceModeSync), w.account(t, "L.OTHER", ledger.KindLiability, ledger.BalanceModeSync)
	w.post(t, vault, other, 1_00)
	corrupt(t, `UPDATE ledger.balances SET posted = posted + 1 WHERE account_id = $1`, other)
	w.run(t)
	found := w.page(t, reconciliation.BreakOpen, "", 10).Breaks
	if len(found) != 1 || found[0].Owner != "finance" || found[0].Reason != "" || found[0].Resolution != nil || found[0].StatementID != (ids.ID{}) {
		t.Fatalf("breaks = %+v", found)
	}

	if err := w.resolve(t, w.entity, found[0].ID, "looked at it"); err != nil {
		t.Fatal(err)
	}
	w.run(t)

	if again := w.page(t, reconciliation.BreakOpen, "", 10).Breaks; len(again) != 1 || again[0].ID == found[0].ID {
		t.Fatalf("breaks = %+v, want a new one", again)
	}
}

// TestBreaksComeInPages: newest first, by status, by entity, with a cursor that holds.
func TestBreaksComeInPages(t *testing.T) {
	w, other := newWorld(t), newWorld(t)
	w.imported(t, w.statement(w.line(w.tag("A"), reconciliation.Outgoing, 1_00), w.line(w.tag("B"), reconciliation.Outgoing, 2_00), w.line(w.tag("C"), reconciliation.Outgoing, 3_00)))
	other.imported(t, other.statement(other.line(other.tag("X"), reconciliation.Outgoing, 1_00)))

	first := w.page(t, "", "", 2)
	if len(first.Breaks) != 2 || first.Next == "" {
		t.Fatalf("first page = %+v", first)
	}
	second := w.page(t, "", first.Next, 2)
	if len(second.Breaks) != 1 || second.Next != "" {
		t.Fatalf("second page = %+v", second)
	}
	seen := map[ids.ID]bool{first.Breaks[0].ID: true, first.Breaks[1].ID: true, second.Breaks[0].ID: true}
	if len(seen) != 3 {
		t.Errorf("pages repeat a break: %v", seen)
	}
	if first.Breaks[0].ID.String() <= first.Breaks[1].ID.String() {
		t.Errorf("first page is not newest first: %+v", first.Breaks)
	}
	if none := w.page(t, reconciliation.BreakResolved, "", 2); len(none.Breaks) != 0 {
		t.Errorf("resolved = %+v, want none", none)
	}
	if read := w.page(t, reconciliation.BreakOpen, "", 1).Breaks; len(read) != 1 || read[0].StatementID == (ids.ID{}) {
		t.Errorf("a statement's break = %+v, want its statement", read)
	}
}

// TestABreakOnlyGetsResolved: nothing else of it changes, for the owner either, and it is resolved once; a statement
// does not change at all.
func TestABreakOnlyGetsResolved(t *testing.T) {
	w := newWorld(t)
	w.imported(t, w.statement(w.line(w.tag("A"), reconciliation.Outgoing, 1_00), w.line(w.tag("B"), reconciliation.Outgoing, 1_00)))
	breaks := w.page(t, "", "", 10).Breaks
	if err := w.resolve(t, w.entity, breaks[0].ID, "cause"); err != nil {
		t.Fatal(err)
	}

	expectForbidden(t, `UPDATE recon.breaks SET cause = 'other' WHERE id = $1`, breaks[0].ID)
	expectForbidden(t, `UPDATE recon.breaks SET status = 'resolved', cause = 'c', resolved_by = 'b', difference_units = 9 WHERE id = $1`, breaks[1].ID)
	expectForbidden(t, `DELETE FROM recon.breaks WHERE id = $1`, breaks[1].ID)
	expectForbidden(t, `UPDATE recon.statements SET line_count = 9 WHERE legal_entity_id = $1`, w.entity)
	expectForbidden(t, `DELETE FROM recon.statements WHERE legal_entity_id = $1`, w.entity)
	expectForbidden(t, `TRUNCATE recon.statements CASCADE`)
	expectInvalid(t, `INSERT INTO recon.breaks (id, legal_entity_id, control, subject, currency, difference_units)
		VALUES (gen_random_uuid(), $1, 'statement_match', 'no-statement', 'COP', 1)`, w.entity)

	if _, err := testPool.Exec(t.Context(), `UPDATE recon.breaks SET difference_units = 9 WHERE id = $1`, breaks[1].ID); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("the application changing a difference: %v, want permission denied", err)
	}
}

// expectInvalid runs statement as the owner, which a CHECK refuses.
func expectInvalid(t *testing.T, statement string, args ...any) {
	t.Helper()
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `SAVEPOINT checked`); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), statement, args...)
		if err == nil || !strings.Contains(err.Error(), "breaks_of_a_statement") {
			t.Errorf("%s: %v, want the CHECK", statement, err)
		}
		_, err = tx.Exec(t.Context(), `ROLLBACK TO SAVEPOINT checked`)
		return err
	})
}
