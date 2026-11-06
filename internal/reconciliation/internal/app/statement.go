package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/payments"
)

// StatementMatch is the "control" of the breaks a rail's statement opens (FR-34).
const StatementMatch Control = "statement_match"

// Reason is why a line and an order did not match.
type Reason string

// The reasons of a statement's exception.
const (
	ReasonAmountMismatch   Reason = "amount_mismatch"    // they have the same id and direction, in another amount
	ReasonLineWithoutOrder Reason = "line_without_order" // the rail moved something the core does not know
	ReasonOrderNotSettled  Reason = "order_not_settled"  // the rail moved an order the core did not settle
	ReasonOrderWithoutLine Reason = "order_without_line" // the core has an order of the day the rail does not
)

// Direction is which way a line went, the same as a payment order's.
type Direction string

// Directions of a line.
const (
	Incoming Direction = "incoming"
	Outgoing Direction = "outgoing"
)

const (
	maxRef   = 35 // the ISO 20022 limit of an identifier
	maxLines = 5000
)

// Errors of a statement.
var (
	// ErrInvalidStatement means a statement of an unknown rail, without a reference or a date or its lines, of
	// more than 5000 lines, or with a line without an id, with an amount that is not positive, of another
	// direction or booked on another day.
	ErrInvalidStatement = errors.New("invalid statement")
	// ErrDuplicateStatement means the entity already imported that statement of the rail.
	ErrDuplicateStatement = errors.New("the statement was already imported")
)

// StatementLine is a movement the rail reports.
type StatementLine struct {
	EndToEndID  string
	Direction   Direction
	Amount      money.Amount
	BookingDate time.Time
}

// Statement is what a rail reports of one day, camt.053 simplified.
type Statement struct {
	ID, LegalEntityID ids.ID
	Rail, Ref         string
	Date              time.Time
	ImportedBy        string
	Lines             []StatementLine
}

// Order is a payment order as a statement sees it. Settled says the core booked it, and Expected that it is of
// the statement's day, so the rail should report it.
type Order struct {
	EndToEndID        string
	Direction         Direction
	Amount            money.Amount
	Settled, Expected bool
}

// ImportResult is what an import found.
type ImportResult struct {
	ID                         ids.ID
	Lines, Matched, Exceptions int
}

// Importing is what importing a statement needs.
type Importing interface {
	opener
	// RecordStatement stores s: ErrDuplicateStatement if the entity already imported it.
	RecordStatement(ctx context.Context, s Statement) error
	// OrdersOf are the orders of s's rail with the id of one of its lines, and those it should report, always in
	// the same order.
	OrdersOf(ctx context.Context, s Statement) ([]Order, error)
}

// Import takes a rail's statement of a day and opens a break for each exception (FR-34), owned by who imported
// it: a statement goes in once, by its reference. One that does not hold is ErrInvalidStatement.
func Import(ctx context.Context, p Importing, s Statement) (ImportResult, error) {
	if err := checkStatement(s); err != nil {
		return ImportResult{}, err
	}
	s.ID = ids.New()
	if err := p.RecordStatement(ctx, s); err != nil {
		return ImportResult{}, err
	}
	orders, err := p.OrdersOf(ctx, s)
	if err != nil {
		return ImportResult{}, err
	}
	findings, matched, err := Match(s.LegalEntityID, s.Lines, orders)
	if err != nil {
		return ImportResult{}, err
	}
	for _, f := range findings {
		b := Break{ID: ids.New(), Control: StatementMatch, Finding: f, Owner: s.ImportedBy, StatementID: s.ID}
		if err := open(ctx, p, b); err != nil {
			return ImportResult{}, err
		}
	}
	return ImportResult{ID: s.ID, Lines: len(s.Lines), Matched: matched, Exceptions: len(findings)}, nil
}

func checkStatement(s Statement) error {
	switch {
	case s.Rail != payments.RailSimBreB:
		return fmt.Errorf("%w: unknown rail", ErrInvalidStatement)
	case !validRef(s.Ref):
		return fmt.Errorf("%w: its reference is text of 1 to %d characters", ErrInvalidStatement, maxRef)
	case s.Date.IsZero():
		return fmt.Errorf("%w: it has no date", ErrInvalidStatement)
	case s.Lines == nil || len(s.Lines) > maxLines:
		return fmt.Errorf("%w: it has its lines, up to %d", ErrInvalidStatement, maxLines)
	}
	for _, l := range s.Lines {
		if err := checkLine(l, s.Date); err != nil {
			return err
		}
	}
	return nil
}

func validRef(s string) bool { return strings.TrimSpace(s) != "" && validText(s, maxRef) }

func checkLine(l StatementLine, day time.Time) error {
	switch {
	case !validRef(l.EndToEndID):
		return fmt.Errorf("%w: an end-to-end id is text of 1 to %d characters", ErrInvalidStatement, maxRef)
	case l.Amount.Sign() <= 0:
		return fmt.Errorf("%w: line %s: the amount must be positive", ErrInvalidStatement, l.EndToEndID)
	case l.Direction != Incoming && l.Direction != Outgoing:
		return fmt.Errorf("%w: line %s: another direction", ErrInvalidStatement, l.EndToEndID)
	case !l.BookingDate.Equal(day):
		return fmt.Errorf("%w: line %s is booked on another day", ErrInvalidStatement, l.EndToEndID)
	}
	return nil
}

// pairing is which order each line took: lines may be left without one, and orders untaken.
type pairing struct {
	orders []Order
	byKey  map[string][]int // indexes of orders, by subject
	taken  []bool
}

func subjectOf(d Direction, e2e string) string { return string(d) + "/" + e2e }

func newPairing(orders []Order) pairing {
	p := pairing{orders: orders, byKey: make(map[string][]int, len(orders)), taken: make([]bool, len(orders))}
	for i, o := range orders {
		key := subjectOf(o.Direction, o.EndToEndID)
		p.byKey[key] = append(p.byKey[key], i)
	}
	return p
}

// take takes the first untaken order of l's id and direction that fits, and returns its index, or -1.
func (p pairing) take(l StatementLine, fits func(Order) bool) int {
	for _, i := range p.byKey[subjectOf(l.Direction, l.EndToEndID)] {
		if !p.taken[i] && fits(p.orders[i]) {
			p.taken[i] = true
			return i
		}
	}
	return -1
}

// Match pairs each line with an order of its id and direction and returns the exceptions of both sides and how
// many lines matched. Every line first looks for the order of its exact amount, preferring one of the day, and
// only then the lines left take what remains, by currency and then by day: two banks can use the same id. The
// lines go in a fixed order, so the one they came in does not change the result. An order of the statement's day
// no line took is an exception by its amount, negative: the core has it and the rail does not.
func Match(entity ids.ID, lines []StatementLine, orders []Order) ([]Finding, int, error) {
	lines = slices.SortedStableFunc(slices.Values(lines), compareLines)
	p := newPairing(orders)
	paired := make([]int, len(lines))
	for i, l := range lines {
		paired[i] = p.takeExact(l)
	}
	for i, l := range lines {
		if paired[i] < 0 {
			paired[i] = p.takeAny(l)
		}
	}
	var findings []Finding
	matched := 0
	for i, l := range lines {
		f, wrong, err := p.exception(entity, l, paired[i])
		if err != nil {
			return nil, 0, err
		}
		if !wrong {
			matched++
			continue
		}
		findings = append(findings, f)
	}
	left, err := p.unreported(entity)
	if err != nil {
		return nil, 0, err
	}
	return append(findings, left...), matched, nil
}

func (p pairing) takeExact(l StatementLine) int {
	if i := p.take(l, func(o Order) bool { return o.Amount == l.Amount && o.Expected }); i >= 0 {
		return i
	}
	return p.take(l, func(o Order) bool { return o.Amount == l.Amount })
}

func (p pairing) takeAny(l StatementLine) int {
	sameCurrency := func(o Order) bool { return o.Amount.Currency() == l.Amount.Currency() }
	for _, fits := range []func(Order) bool{
		func(o Order) bool { return sameCurrency(o) && o.Expected },
		sameCurrency,
		func(o Order) bool { return o.Expected },
		func(Order) bool { return true },
	} {
		if i := p.take(l, fits); i >= 0 {
			return i
		}
	}
	return -1
}

func compareLines(a, b StatementLine) int {
	return cmp.Or(
		strings.Compare(a.EndToEndID, b.EndToEndID), strings.Compare(string(a.Direction), string(b.Direction)),
		strings.Compare(a.Amount.Currency().Code(), b.Amount.Currency().Code()), cmp.Compare(a.Amount.Units(), b.Amount.Units()),
	)
}

// exception is what is wrong with l and the order at index at, -1 if it found none, and false if they match. A
// difference is the line's minus the order's, or the line's amount if they are in different currencies, since
// they cannot subtract.
func (p pairing) exception(entity ids.ID, l StatementLine, at int) (Finding, bool, error) {
	f := Finding{LegalEntityID: entity, Subject: subjectOf(l.Direction, l.EndToEndID), Difference: l.Amount}
	if at < 0 {
		f.Reason = ReasonLineWithoutOrder
		return f, true, nil
	}
	o := p.orders[at]
	switch {
	case o.Amount == l.Amount && o.Settled:
		return Finding{}, false, nil
	case o.Amount == l.Amount:
		f.Reason = ReasonOrderNotSettled
		return f, true, nil
	}
	f.Reason = ReasonAmountMismatch
	if o.Amount.Currency() != l.Amount.Currency() {
		return f, true, nil
	}
	difference, err := l.Amount.Sub(o.Amount)
	if err != nil {
		return Finding{}, false, fmt.Errorf("difference of line %s: %w", l.EndToEndID, err)
	}
	f.Difference = difference
	return f, true, nil
}

// unreported are the orders of the statement's day no line took.
func (p pairing) unreported(entity ids.ID) ([]Finding, error) {
	var findings []Finding
	for i, o := range p.orders {
		if p.taken[i] || !o.Expected {
			continue
		}
		difference, err := o.Amount.Neg()
		if err != nil {
			return nil, fmt.Errorf("order %s: %w", o.EndToEndID, err)
		}
		findings = append(findings, Finding{
			LegalEntityID: entity, Subject: subjectOf(o.Direction, o.EndToEndID), Reason: ReasonOrderWithoutLine, Difference: difference,
		})
	}
	return findings, nil
}
