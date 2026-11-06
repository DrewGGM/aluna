package app_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/reconciliation/internal/app"
)

func cop(t *testing.T, units int64) money.Amount {
	t.Helper()
	c, err := money.CurrencyOf("COP")
	if err != nil {
		t.Fatal(err)
	}
	return money.New(units, c)
}

func line(t *testing.T, e2e string, units int64) app.StatementLine {
	t.Helper()
	return app.StatementLine{EndToEndID: e2e, Direction: app.Outgoing, Amount: cop(t, units), BookingDate: statementDay}
}

// settled is a settled outgoing order of the statement's day.
func settled(t *testing.T, e2e string, units int64) app.Order {
	t.Helper()
	return app.Order{EndToEndID: e2e, Direction: app.Outgoing, Amount: cop(t, units), Settled: true, Expected: true}
}

var statementDay = time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)

// summary is a finding as these tests compare it.
type summary struct {
	subject string
	reason  app.Reason
	units   int64
}

func match(t *testing.T, lines []app.StatementLine, orders []app.Order) ([]summary, int) {
	t.Helper()
	findings, matched, err := app.Match(ids.New(), lines, orders)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]summary, len(findings))
	for i, f := range findings {
		got[i] = summary{f.Subject, f.Reason, f.Difference.Units()}
	}
	return got, matched
}

// matchCase is a statement's two sides and what they should give.
type matchCase struct {
	lines   []app.StatementLine
	orders  []app.Order
	want    []summary
	matched int
}

func expectMatches(t *testing.T, cases map[string]matchCase) {
	t.Helper()
	for name, c := range cases {
		got, matched := match(t, c.lines, c.orders)
		if !slices.Equal(got, c.want) || matched != c.matched {
			t.Errorf("%s: findings %v and %d matched, want %v and %d", name, got, matched, c.want, c.matched)
		}
	}
}

// TestALineGoesToTheOrderOfItsAmount (US-9): a difference is the line's minus the order's, and two banks can use
// the same id.
func TestALineGoesToTheOrderOfItsAmount(t *testing.T) {
	usd, err := money.CurrencyOf("USD")
	if err != nil {
		t.Fatal(err)
	}
	inDollars := settled(t, "E2E", 100)
	inDollars.Amount = money.New(100, usd)
	expectMatches(t, map[string]matchCase{
		"a line and its order": {[]app.StatementLine{line(t, "E2E", 100)}, []app.Order{settled(t, "E2E", 100)}, nil, 1},
		"a larger amount": {
			[]app.StatementLine{line(t, "E2E", 130)},
			[]app.Order{settled(t, "E2E", 100)},
			[]summary{{"outgoing/E2E", app.ReasonAmountMismatch, 30}},
			0,
		},
		"a smaller amount": {
			[]app.StatementLine{line(t, "E2E", 70)},
			[]app.Order{settled(t, "E2E", 100)},
			[]summary{{"outgoing/E2E", app.ReasonAmountMismatch, -30}},
			0,
		},
		"another currency": {
			[]app.StatementLine{line(t, "E2E", 100)},
			[]app.Order{inDollars},
			[]summary{{"outgoing/E2E", app.ReasonAmountMismatch, 100}},
			0,
		},
		"two orders with the id": {
			[]app.StatementLine{line(t, "E2E", 200), line(t, "E2E", 100)}, []app.Order{settled(t, "E2E", 100), settled(t, "E2E", 200)}, nil, 2,
		},
	})
}

// TestTheOrderOfTheLinesChangesNothing (FR-34): a line with no order of its amount does not take the one of a later
// line, and in the same currency if it can.
func TestTheOrderOfTheLinesChangesNothing(t *testing.T) {
	usd, err := money.CurrencyOf("USD")
	if err != nil {
		t.Fatal(err)
	}
	inDollars := settled(t, "E2E", 100)
	inDollars.Amount = money.New(100, usd)
	expectMatches(t, map[string]matchCase{
		"the mismatch first": {
			[]app.StatementLine{line(t, "E2E", 300), line(t, "E2E", 100)},
			[]app.Order{settled(t, "E2E", 100), settled(t, "E2E", 200)},
			[]summary{{"outgoing/E2E", app.ReasonAmountMismatch, 100}},
			1,
		},
		"the mismatch last": {
			[]app.StatementLine{line(t, "E2E", 100), line(t, "E2E", 300)},
			[]app.Order{settled(t, "E2E", 100), settled(t, "E2E", 200)},
			[]summary{{"outgoing/E2E", app.ReasonAmountMismatch, 100}},
			1,
		},
		"the same currency before another": {
			[]app.StatementLine{line(t, "E2E", 100)},
			[]app.Order{inDollars, settled(t, "E2E", 90)},
			[]summary{{"outgoing/E2E", app.ReasonAmountMismatch, 10}, {"outgoing/E2E", app.ReasonOrderWithoutLine, -100}},
			0,
		},
		"two mismatches, one way": {
			[]app.StatementLine{line(t, "E2E", 300), line(t, "E2E", 150)},
			[]app.Order{settled(t, "E2E", 100), settled(t, "E2E", 200)},
			[]summary{{"outgoing/E2E", app.ReasonAmountMismatch, 50}, {"outgoing/E2E", app.ReasonAmountMismatch, 100}},
			0,
		},
		"two mismatches, the other way": {
			[]app.StatementLine{line(t, "E2E", 150), line(t, "E2E", 300)},
			[]app.Order{settled(t, "E2E", 100), settled(t, "E2E", 200)},
			[]summary{{"outgoing/E2E", app.ReasonAmountMismatch, 50}, {"outgoing/E2E", app.ReasonAmountMismatch, 100}},
			0,
		},
	})
}

// TestTheExactOrderComesFirst: every line looks for its amount before any line takes what remains, and of two
// orders the one of the day goes first.
func TestTheExactOrderComesFirst(t *testing.T) {
	expectMatches(t, map[string]matchCase{
		"though not the first": {
			[]app.StatementLine{line(t, "E2E", 100), line(t, "E2E", 999)},
			[]app.Order{settled(t, "E2E", 200), settled(t, "E2E", 100)},
			[]summary{{"outgoing/E2E", app.ReasonAmountMismatch, 799}},
			1,
		},
		"the order of the day before an equal one": {
			[]app.StatementLine{line(t, "E2E", 100)}, []app.Order{older(t, 100), settled(t, "E2E", 100)}, nil, 1,
		},
		"the order of the day before an older one": {
			[]app.StatementLine{line(t, "E2E", 150)},
			[]app.Order{older(t, 100), settled(t, "E2E", 140)},
			[]summary{{"outgoing/E2E", app.ReasonAmountMismatch, 10}},
			0,
		},
	})
}

// TestALeftoverLinePrefersItsCurrencyThenTheDay: what remains goes to the likeliest order, and any order is better
// than saying the core does not know the movement.
func TestALeftoverLinePrefersItsCurrencyThenTheDay(t *testing.T) {
	dollars := func(o app.Order) app.Order {
		usd, err := money.CurrencyOf("USD")
		if err != nil {
			t.Fatal(err)
		}
		o.Amount = money.New(o.Amount.Units(), usd)
		return o
	}
	inDollars := line(t, "E2E", 100)
	inDollars.Amount = dollars(settled(t, "E2E", 100)).Amount
	expectMatches(t, map[string]matchCase{
		"the same amount in two currencies goes by currency": {
			[]app.StatementLine{inDollars, line(t, "E2E", 100)},
			[]app.Order{settled(t, "E2E", 5)},
			[]summary{{"outgoing/E2E", app.ReasonAmountMismatch, 95}, {"outgoing/E2E", app.ReasonLineWithoutOrder, 100}},
			0,
		},
		"its currency, though of another day": {
			[]app.StatementLine{line(t, "E2E", 100)},
			[]app.Order{dollars(settled(t, "E2E", 100)), older(t, 90)},
			[]summary{{"outgoing/E2E", app.ReasonAmountMismatch, 10}, {"outgoing/E2E", app.ReasonOrderWithoutLine, -100}},
			0,
		},
		"the day, in another currency": {
			[]app.StatementLine{line(t, "E2E", 100)},
			[]app.Order{dollars(older(t, 90)), dollars(settled(t, "E2E", 80))},
			[]summary{{"outgoing/E2E", app.ReasonAmountMismatch, 100}},
			0,
		},
		"another day and another currency": {
			[]app.StatementLine{line(t, "E2E", 100)},
			[]app.Order{dollars(older(t, 90))},
			[]summary{{"outgoing/E2E", app.ReasonAmountMismatch, 100}},
			0,
		},
	})
}

// older is a settled order of another day.
func older(t *testing.T, units int64) app.Order {
	t.Helper()
	o := settled(t, "E2E", units)
	o.Expected = false
	return o
}

// TestALineWithoutItsOrderIsAnException (FR-34): one of the other direction is another order, and one the core did
// not settle is no match.
func TestALineWithoutItsOrderIsAnException(t *testing.T) {
	notSettled := settled(t, "E2E", 100)
	notSettled.Settled = false
	incoming := settled(t, "E2E", 100)
	incoming.Direction = app.Incoming
	expectMatches(t, map[string]matchCase{
		"no order": {[]app.StatementLine{line(t, "E2E", 100)}, nil, []summary{{"outgoing/E2E", app.ReasonLineWithoutOrder, 100}}, 0},
		"an order of the other direction": {
			[]app.StatementLine{line(t, "E2E", 100)},
			[]app.Order{incoming},
			[]summary{{"outgoing/E2E", app.ReasonLineWithoutOrder, 100}, {"incoming/E2E", app.ReasonOrderWithoutLine, -100}},
			0,
		},
		"an order the core did not settle": {
			[]app.StatementLine{line(t, "E2E", 100)},
			[]app.Order{notSettled},
			[]summary{{"outgoing/E2E", app.ReasonOrderNotSettled, 100}},
			0,
		},
		"the same line twice": {
			[]app.StatementLine{line(t, "E2E", 100), line(t, "E2E", 100)},
			[]app.Order{settled(t, "E2E", 100)},
			[]summary{{"outgoing/E2E", app.ReasonLineWithoutOrder, 100}},
			1,
		},
	})
}

// TestAnOrderOfTheDayNeedsItsLine (FR-34): one of another day does not, but matches if it has one.
func TestAnOrderOfTheDayNeedsItsLine(t *testing.T) {
	earlier := settled(t, "E2E", 100)
	earlier.Expected = false
	expectMatches(t, map[string]matchCase{
		"of the day":          {nil, []app.Order{settled(t, "E2E", 100)}, []summary{{"outgoing/E2E", app.ReasonOrderWithoutLine, -100}}, 0},
		"of another day":      {nil, []app.Order{earlier}, nil, 0},
		"of another day, too": {[]app.StatementLine{line(t, "E2E", 100)}, []app.Order{earlier}, nil, 1},
	})
}

// importing is an Importing in memory.
type importing struct {
	*memory
	orders    []app.Order
	recorded  []app.Statement
	asked     []string
	duplicate bool
	ordersErr error
}

func (p *importing) RecordStatement(_ context.Context, s app.Statement) error {
	if p.duplicate {
		return app.ErrDuplicateStatement
	}
	p.recorded = append(p.recorded, s)
	return nil
}

func (p *importing) failingOrders() *importing { p.ordersErr = errFailed; return p }

func (p *importing) OrdersOf(_ context.Context, s app.Statement) ([]app.Order, error) {
	for _, l := range s.Lines {
		p.asked = append(p.asked, l.EndToEndID)
	}
	return p.orders, p.ordersErr
}

func statement(t *testing.T, lines ...app.StatementLine) app.Statement {
	t.Helper()
	return app.Statement{
		LegalEntityID: ids.New(), Rail: "simbreb", Ref: "STMT-1", Date: statementDay, ImportedBy: "finance-a",
		Lines: append([]app.StatementLine{}, lines...),
	}
}

// TestAnImportOpensABreakForEachException: the owner is who imported the statement, and the result counts the
// lines, the matched and the exceptions.
func TestAnImportOpensABreakForEachException(t *testing.T) {
	p := &importing{memory: newMemory(), orders: []app.Order{settled(t, "GOOD", 100), settled(t, "BAD", 100)}}
	s := statement(t, line(t, "GOOD", 100), line(t, "BAD", 130))

	got, err := app.Import(t.Context(), p, s)
	if err != nil {
		t.Fatal(err)
	}

	if want := (app.ImportResult{ID: got.ID, Lines: 2, Matched: 1, Exceptions: 1}); got != want || got.ID == (ids.ID{}) {
		t.Errorf("result = %+v, want %+v", got, want)
	}
	s.ID = got.ID
	expectBreak(t, p.breaks, s)
	if want := []string{"outgoing/BAD"}; !slices.Equal(p.published, want) {
		t.Errorf("published = %v, want %v", p.published, want)
	}
}

// expectBreak checks that the only break is the one of line BAD of s.
func expectBreak(t *testing.T, breaks []app.Break, s app.Statement) {
	t.Helper()
	if len(breaks) != 1 {
		t.Fatalf("breaks = %+v, want one", breaks)
	}
	b := breaks[0]
	if b.Control != app.StatementMatch || b.Owner != "finance-a" || b.StatementID != s.ID || b.LegalEntityID != s.LegalEntityID ||
		b.Subject != "outgoing/BAD" || b.Reason != app.ReasonAmountMismatch || b.Difference.Units() != 30 {
		t.Errorf("break = %+v", b)
	}
}

// TestAStatementImportedTwiceChangesNothing: its reference is taken before anything opens.
func TestAStatementImportedTwiceChangesNothing(t *testing.T) {
	p := &importing{memory: newMemory(), duplicate: true}
	if _, err := app.Import(t.Context(), p, statement(t, line(t, "E2E", 100))); !errors.Is(err, app.ErrDuplicateStatement) {
		t.Fatalf("Import = %v, want ErrDuplicateStatement", err)
	}
	if len(p.breaks) != 0 {
		t.Errorf("breaks = %+v, want none", p.breaks)
	}
}

func TestAnInvalidStatementIsRejected(t *testing.T) {
	otherDay := line(t, "E2E", 100)
	otherDay.BookingDate = statementDay.AddDate(0, 0, 1)
	sideways := line(t, "E2E", 100)
	sideways.Direction = "sideways"
	with := func(change func(*app.Statement)) app.Statement {
		s := statement(t)
		change(&s)
		return s
	}
	for name, s := range map[string]app.Statement{
		"an unknown rail":   with(func(s *app.Statement) { s.Rail = "swift" }),
		"no reference":      with(func(s *app.Statement) { s.Ref = "" }),
		"a blank reference": with(func(s *app.Statement) { s.Ref = "   " }),
		"a NUL reference":   with(func(s *app.Statement) { s.Ref = "A\x00B" }),
		"bad UTF-8":         with(func(s *app.Statement) { s.Ref = "A\xffB" }),
		"a long reference":  with(func(s *app.Statement) { s.Ref = strings.Repeat("A", 36) }),
		"no date":           with(func(s *app.Statement) { s.Date = time.Time{} }),
		"no lines":          with(func(s *app.Statement) { s.Lines = nil }),
		"too many lines":    with(func(s *app.Statement) { s.Lines = slices.Repeat([]app.StatementLine{line(t, "E2E", 1)}, 5001) }),
		"a NUL end-to-end":  statement(t, line(t, "E\x00", 100)),
		"no end-to-end id":  statement(t, line(t, "", 100)),
		"a long end-to-end": statement(t, line(t, "1234567890123456789012345678901234567", 100)),
		"a zero amount":     statement(t, line(t, "E2E", 0)),
		"a negative amount": statement(t, line(t, "E2E", -1)),
		"another direction": statement(t, sideways),
		"another day":       statement(t, otherDay),
	} {
		p := &importing{memory: newMemory()}
		if _, err := app.Import(t.Context(), p, s); !errors.Is(err, app.ErrInvalidStatement) {
			t.Errorf("%s: Import = %v, want ErrInvalidStatement", name, err)
		}
		if len(p.recorded) != 0 {
			t.Errorf("%s: recorded it", name)
		}
	}
}

// breaksStore is a Resolving in memory.
type breaksStore struct {
	found     app.Break
	missing   error
	at        time.Time
	resolved  []app.Break
	announced []ids.ID
}

func (s *breaksStore) LockBreak(_ context.Context, _, _ ids.ID) (app.Break, error) {
	return s.found, s.missing
}

func (s *breaksStore) ResolveBreak(_ context.Context, b app.Break) (time.Time, error) {
	s.resolved = append(s.resolved, b)
	return s.at, nil
}

func (s *breaksStore) PublishResolved(_ context.Context, b app.Break) error {
	s.announced = append(s.announced, b.ID)
	return nil
}

// TestTheLimitsOfAStatementAreIn: the largest of each is valid.
func TestTheLimitsOfAStatementAreIn(t *testing.T) {
	full := statement(t, slices.Repeat([]app.StatementLine{line(t, strings.Repeat("E", 35), 1)}, 5000)...)
	full.Ref = strings.Repeat("R", 35)
	if _, err := app.Import(t.Context(), &importing{memory: newMemory()}, full); err != nil {
		t.Fatal(err)
	}
	s := &breaksStore{found: app.Break{Status: app.BreakOpen}}
	if _, err := app.Resolve(t.Context(), s, ids.New(), ids.New(), strings.Repeat("c", 1000), "finance-a"); err != nil {
		t.Fatal(err)
	}
}

func TestAnEmptyStatementIsAStatement(t *testing.T) {
	p := &importing{memory: newMemory()}
	got, err := app.Import(t.Context(), p, statement(t))
	if err != nil {
		t.Fatal(err)
	}
	if got.Lines != 0 || got.ID == (ids.ID{}) {
		t.Errorf("result = %+v", got)
	}
}

func TestAFailureToReadTheOrdersStopsTheImport(t *testing.T) {
	p := (&importing{memory: newMemory()}).failingOrders()
	if _, err := app.Import(t.Context(), p, statement(t, line(t, "E2E", 100))); !errors.Is(err, errFailed) {
		t.Fatalf("Import = %v, want %v", err, errFailed)
	}
}

func TestABreakIsResolvedWithItsCause(t *testing.T) {
	id := ids.New()
	s := &breaksStore{found: app.Break{ID: id, Status: app.BreakOpen}, at: statementDay}

	b, err := app.Resolve(t.Context(), s, ids.New(), id, "duplicate rail line", "finance-a")
	if err != nil {
		t.Fatal(err)
	}

	want := app.Resolution{Cause: "duplicate rail line", By: "finance-a", At: statementDay}
	if b.Status != app.BreakResolved || b.Resolution == nil || *b.Resolution != want {
		t.Errorf("break = %+v, want the resolution %+v", b, want)
	}
	if len(s.resolved) != 1 || s.resolved[0].Resolution.Cause != want.Cause || !slices.Equal(s.announced, []ids.ID{id}) {
		t.Errorf("resolved %+v, announced %v", s.resolved, s.announced)
	}
}

func TestAResolutionNeedsACause(t *testing.T) {
	for _, cause := range []string{"", "   ", "a\x00b", strings.Repeat("a", 1001)} {
		s := &breaksStore{found: app.Break{Status: app.BreakOpen}}
		if _, err := app.Resolve(t.Context(), s, ids.New(), ids.New(), cause, "finance-a"); !errors.Is(err, app.ErrInvalidResolution) {
			t.Errorf("cause of %d bytes: %v, want ErrInvalidResolution", len(cause), err)
		}
		if len(s.resolved) != 0 {
			t.Errorf("cause of %d bytes resolved it", len(cause))
		}
	}
}

func TestABreakIsResolvedOnce(t *testing.T) {
	s := &breaksStore{found: app.Break{Status: app.BreakResolved}}
	if _, err := app.Resolve(t.Context(), s, ids.New(), ids.New(), "again", "finance-a"); !errors.Is(err, app.ErrBreakResolved) {
		t.Fatalf("Resolve = %v, want ErrBreakResolved", err)
	}
	s = &breaksStore{missing: app.ErrBreakNotFound}
	if _, err := app.Resolve(t.Context(), s, ids.New(), ids.New(), "cause", "finance-a"); !errors.Is(err, app.ErrBreakNotFound) {
		t.Fatalf("Resolve = %v, want ErrBreakNotFound", err)
	}
}

// shelf is a BreakReader of breaks in memory, newest first.
type shelf struct {
	breaks  []app.Break
	status  app.BreakStatus
	before  ids.ID
	limit   int
	failing error
}

func (s *shelf) BreaksBefore(_ context.Context, _ ids.ID, status app.BreakStatus, before ids.ID, limit int) ([]app.Break, error) {
	s.status, s.before, s.limit = status, before, limit
	return s.breaks[:min(limit, len(s.breaks))], s.failing
}

func TestAPageOfBreaksHasItsNext(t *testing.T) {
	newest, middle, oldest := app.Break{ID: ids.New()}, app.Break{ID: ids.New()}, app.Break{ID: ids.New()}
	s := &shelf{breaks: []app.Break{newest, middle, oldest}}

	page, err := app.Breaks(t.Context(), s, ids.New(), app.BreakOpen, "", 2)
	if err != nil {
		t.Fatal(err)
	}

	if len(page.Breaks) != 2 || page.Breaks[1].ID != middle.ID || page.Next != middle.ID.String() || s.status != app.BreakOpen {
		t.Errorf("page = %+v with status %s", page, s.status)
	}

	s.breaks = []app.Break{oldest}
	last, err := app.Breaks(t.Context(), s, ids.New(), "", page.Next, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(last.Breaks) != 1 || last.Next != "" || s.before != middle.ID {
		t.Errorf("last page = %+v after %s", last, s.before)
	}
}

func TestAnInvalidPageIsRejected(t *testing.T) {
	for name, c := range map[string]struct {
		status app.BreakStatus
		cursor string
		limit  int
	}{
		"no breaks":       {"", "", 0},
		"a negative page": {"", "", -1},
		"too many":        {"", "", 201},
		"another status":  {"bogus", "", 1},
		"another cursor":  {"", "nope", 1},
	} {
		s := &shelf{}
		if _, err := app.Breaks(t.Context(), s, ids.New(), c.status, c.cursor, c.limit); !errors.Is(err, app.ErrInvalidPage) {
			t.Errorf("%s: %v, want ErrInvalidPage", name, err)
		}
		if s.limit != 0 {
			t.Errorf("%s reached the store", name)
		}
	}
	if _, err := app.Breaks(t.Context(), &shelf{}, ids.New(), "", "", 200); err != nil {
		t.Errorf("a full page: %v", err)
	}
}

// TestAPageJustFullHasNoNext: limit breaks and no more are the last page.
func TestAPageJustFullHasNoNext(t *testing.T) {
	s := &shelf{breaks: []app.Break{{ID: ids.New()}, {ID: ids.New()}}}
	page, err := app.Breaks(t.Context(), s, ids.New(), "", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Breaks) != 2 || page.Next != "" {
		t.Errorf("page = %+v, want both and no next", page)
	}
}

func TestAFailureToReadTheBreaksComesBack(t *testing.T) {
	if _, err := app.Breaks(t.Context(), &shelf{failing: errFailed}, ids.New(), "", "", 1); !errors.Is(err, errFailed) {
		t.Fatalf("Breaks = %v, want %v", err, errFailed)
	}
}
