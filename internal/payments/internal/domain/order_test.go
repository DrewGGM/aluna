package domain

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
)

func cop(t *testing.T) money.Currency {
	t.Helper()
	c, err := money.CurrencyOf("COP")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func outgoing(t *testing.T) OutgoingInput {
	t.Helper()
	id := ids.New()
	return OutgoingInput{
		ID: id, LegalEntityID: ids.New(), AccountID: ids.New(), Amount: money.New(5000000, cop(t)), Rail: "breb",
		EndToEndID: NewEndToEndID("ALUNA", id), InstrID: "INSTR-1", TxID: "TX-1",
	}
}

// TestAnOutgoingOrderStartsInitiated (FR-30): a new outgoing order keeps its identifiers, amount and rail and
// starts initiated.
func TestAnOutgoingOrderStartsInitiated(t *testing.T) {
	in := outgoing(t)

	o, err := NewOutgoing(in)

	want := Order{
		ID: in.ID, LegalEntityID: in.LegalEntityID, AccountID: in.AccountID, Direction: Outgoing, Amount: in.Amount, Rail: in.Rail,
		EndToEndID: in.EndToEndID, InstrID: in.InstrID, TxID: in.TxID, Status: StatusInitiated,
	}
	if err != nil || o != want {
		t.Fatalf("new order = %+v (%v), want %+v", o, err, want)
	}
}

// largestID is the id of all ones, the longest in base 36.
func largestID() ids.ID {
	var id ids.ID
	for i := range id {
		id[i] = 0xff
	}
	return id
}

// TestAnyISO20022IdentifierIsTaken (FR-30): an identifier of 35 characters, the longest end-to-end id, and one
// with every character of the restricted set but the slash at its ends are taken.
func TestAnyISO20022IdentifierIsTaken(t *testing.T) {
	in := outgoing(t)
	in.EndToEndID, in.InstrID, in.TxID = NewEndToEndID("ABCDEFGHIJ", largestID()), "a-b?c:d(e).f,g'h+i j/k", strings.Repeat("9", 35)

	if _, err := NewOutgoing(in); err != nil {
		t.Fatalf("identifiers %q, %q and %q = %v, want them taken", in.EndToEndID, in.InstrID, in.TxID, err)
	}
}

// TestAnInvalidOrderIsNotCreated (FR-30): an order needs a positive amount, a rail and ISO 20022 identifiers.
func TestAnInvalidOrderIsNotCreated(t *testing.T) {
	cases := map[string]func(*OutgoingInput){
		"no id":                    func(in *OutgoingInput) { in.ID = ids.ID{} },
		"no entity":                func(in *OutgoingInput) { in.LegalEntityID = ids.ID{} },
		"no account":               func(in *OutgoingInput) { in.AccountID = ids.ID{} },
		"zero amount":              func(in *OutgoingInput) { in.Amount = money.New(0, cop(t)) },
		"negative amount":          func(in *OutgoingInput) { in.Amount = money.New(-1, cop(t)) },
		"no rail":                  func(in *OutgoingInput) { in.Rail = "" },
		"no end-to-end id":         func(in *OutgoingInput) { in.EndToEndID = "" },
		"an end-to-end id of 36":   func(in *OutgoingInput) { in.EndToEndID = strings.Repeat("A", 36) },
		"an instruction id with _": func(in *OutgoingInput) { in.InstrID = "INSTR_1" },
		"a transaction id with //": func(in *OutgoingInput) { in.TxID = "TX//1" },
		"an id that starts with /": func(in *OutgoingInput) { in.TxID = "/TX1" },
		"an id that ends with /":   func(in *OutgoingInput) { in.InstrID = "INSTR1/" },
		"an id with a non-ASCII ñ": func(in *OutgoingInput) { in.EndToEndID = "PAGOÑ1" },
	}
	for name, change := range cases {
		in := outgoing(t)
		change(&in)
		if _, err := NewOutgoing(in); !errors.Is(err, ErrInvalidOrder) {
			t.Errorf("%s = %v, want ErrInvalidOrder", name, err)
		}
	}
}

// transitions are the moves of an order, by name.
var transitions = map[string]func(*Order) error{
	"screen":           (*Order).StartScreening,
	"await compliance": (*Order).AwaitCompliance,
	"hold":             (*Order).Hold,
	"send":             (*Order).Send,
	"settle":           (*Order).Settle,
	"reject":           func(o *Order) error { return o.Reject("insufficient_funds") },
	"return":           (*Order).Return,
	"credit":           (*Order).Credit,
	"suspend":          func(o *Order) error { return o.Suspend("UNKNOWN_ACCOUNT") },
}

// TestTheStateMachine (FR-30): every move the diagram has takes the order where it says, and every other move
// from every status is ErrIllegalTransition and leaves the order as it was.
func TestTheStateMachine(t *testing.T) {
	legal := map[Status]map[string]Status{
		StatusInitiated:        {"screen": StatusScreening},
		StatusScreening:        {"await compliance": StatusPendingScreening, "hold": StatusHeld},
		StatusPendingScreening: {"hold": StatusHeld, "reject": StatusRejected},
		StatusHeld:             {"send": StatusSent, "reject": StatusRejected},
		StatusSent:             {"settle": StatusSettled, "reject": StatusRejected},
		StatusSettled:          {"return": StatusReturned},
		StatusRejected:         {},
		StatusReturned:         {},
		StatusSuspended:        {},
	}
	for from, moves := range legal {
		for name, move := range transitions {
			o, err := NewOutgoing(outgoing(t))
			if err != nil {
				t.Fatal(err)
			}
			o.Status = from
			err = move(&o)
			want, ok := moves[name]
			switch {
			case ok && (err != nil || o.Status != want):
				t.Errorf("%s from %s = %s (%v), want %s", name, from, o.Status, err, want)
			case !ok && (!errors.Is(err, ErrIllegalTransition) || o.Status != from):
				t.Errorf("%s from %s = %s (%v), want ErrIllegalTransition and the order still %s", name, from, o.Status, err, from)
			}
		}
	}
}

func incoming(t *testing.T) IncomingInput {
	t.Helper()
	return IncomingInput{
		ID: ids.New(), LegalEntityID: ids.New(), Amount: money.New(5000000, cop(t)), Rail: "breb",
		EndToEndID: "BANKX20260302000001", InstrID: "INSTR-1", TxID: "TX-1", DebtorAgent: "BANKXCOBB",
	}
}

// TestAnIncomingOrderStartsInitiated (FR-32): a transfer received keeps its identifiers, its amount, its rail and
// the bank that sent it, and starts initiated, without an account until it is resolved.
func TestAnIncomingOrderStartsInitiated(t *testing.T) {
	in := incoming(t)

	o, err := NewIncoming(in)

	want := Order{
		ID: in.ID, LegalEntityID: in.LegalEntityID, Direction: Incoming, Amount: in.Amount, Rail: in.Rail, EndToEndID: in.EndToEndID,
		InstrID: in.InstrID, TxID: in.TxID, DebtorAgent: in.DebtorAgent, Status: StatusInitiated,
	}
	if err != nil || o != want {
		t.Fatalf("new order = %+v (%v), want %+v", o, err, want)
	}
}

// TestAnInvalidIncomingOrderIsNotCreated: without its id or its entity, with an amount that is not positive,
// without its rail, with an identifier that is not ISO 20022 or a debtor agent that is not letters and digits,
// it is ErrInvalidOrder.
func TestAnInvalidIncomingOrderIsNotCreated(t *testing.T) {
	cases := map[string]func(*IncomingInput){
		"no id":                 func(in *IncomingInput) { in.ID = ids.ID{} },
		"no entity":             func(in *IncomingInput) { in.LegalEntityID = ids.ID{} },
		"zero amount":           func(in *IncomingInput) { in.Amount = money.New(0, cop(t)) },
		"no rail":               func(in *IncomingInput) { in.Rail = "" },
		"no end-to-end id":      func(in *IncomingInput) { in.EndToEndID = "" },
		"an instr id too long":  func(in *IncomingInput) { in.InstrID = strings.Repeat("I", 36) },
		"a tx id with a slash":  func(in *IncomingInput) { in.TxID = "/TX" },
		"no debtor agent":       func(in *IncomingInput) { in.DebtorAgent = "" },
		"a debtor agent of 36":  func(in *IncomingInput) { in.DebtorAgent = strings.Repeat("B", 36) },
		"a debtor agent with /": func(in *IncomingInput) { in.DebtorAgent = "BANK/A" },
		"a debtor agent with -": func(in *IncomingInput) { in.DebtorAgent = "BANK-A" },
	}
	for name, change := range cases {
		in := incoming(t)
		change(&in)
		if o, err := NewIncoming(in); !errors.Is(err, ErrInvalidOrder) || o != (Order{}) {
			t.Errorf("%s: %+v (%v), want ErrInvalidOrder", name, o, err)
		}
	}
}

// incomingMoves are the moves of an incoming order: from initiated only.
var incomingMoves = map[Status]map[string]Status{StatusInitiated: {"credit": StatusSettled, "suspend": StatusSuspended}}

// TestTheIncomingStateMachine (FR-32): a transfer received is credited (settled) or goes to suspense, from
// initiated only; every other move from every status is ErrIllegalTransition, and so are those of an outgoing
// order.
func TestTheIncomingStateMachine(t *testing.T) {
	for _, from := range []Status{
		StatusInitiated, StatusScreening, StatusPendingScreening, StatusHeld, StatusSent, StatusSettled,
		StatusRejected, StatusReturned, StatusSuspended,
	} {
		for name, move := range transitions {
			o, err := NewIncoming(incoming(t))
			if err != nil {
				t.Fatal(err)
			}
			o.Status = from
			err = move(&o)
			want, ok := incomingMoves[from][name]
			switch {
			case ok && (err != nil || o.Status != want):
				t.Errorf("%s from %s = %s (%v), want %s", name, from, o.Status, err, want)
			case !ok && (!errors.Is(err, ErrIllegalTransition) || o.Status != from):
				t.Errorf("%s from %s = %s (%v), want ErrIllegalTransition and the order still %s", name, from, o.Status, err, from)
			}
		}
	}
}

// TestASuspenseKeepsItsReason (FR-32): an order goes to suspense saying why, and without a reason it does not.
func TestASuspenseKeepsItsReason(t *testing.T) {
	o, err := NewIncoming(incoming(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Suspend(""); !errors.Is(err, ErrInvalidOrder) || o.Status != StatusInitiated {
		t.Fatalf("suspending without a reason = %v, the order %s, want ErrInvalidOrder and the order still initiated", err, o.Status)
	}

	if err := o.Suspend("ACCOUNT_NOT_ACTIVE"); err != nil || o.Reason != "ACCOUNT_NOT_ACTIVE" {
		t.Fatalf("suspending = %v with reason %q, want ACCOUNT_NOT_ACTIVE", err, o.Reason)
	}
}

// TestARejectionKeepsItsReason (FR-31): a rejected order keeps why, and an illegal rejection does not.
func TestARejectionKeepsItsReason(t *testing.T) {
	o, err := NewOutgoing(outgoing(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Reject("too early"); !errors.Is(err, ErrIllegalTransition) || o.Reason != "" {
		t.Fatalf("rejecting an initiated order = %v with reason %q, want ErrIllegalTransition and none", err, o.Reason)
	}
	o.Status = StatusSent
	if err := o.Reject(""); !errors.Is(err, ErrInvalidOrder) || o.Status != StatusSent {
		t.Fatalf("rejecting without a reason = %v, the order %s, want ErrInvalidOrder and the order still sent", err, o.Status)
	}

	if err := o.Reject("rail_rejected"); err != nil || o.Reason != "rail_rejected" {
		t.Fatalf("rejecting a sent order = %v with reason %q, want rail_rejected", err, o.Reason)
	}
}

// alphanumeric is what NewEndToEndID writes: upper case letters and digits only.
var alphanumeric = regexp.MustCompile(`^[0-9A-Z]+$`)

// expectEndToEndID checks that got, the end-to-end id of id with a prefix that cleans to prefix, is that prefix
// and then the id in idDigits characters, an ISO 20022 identifier of letters and digits.
func expectEndToEndID(t *testing.T, got, prefix string) {
	t.Helper()
	if !strings.HasPrefix(got, prefix) {
		t.Errorf("%q does not start with %q", got, prefix)
	}
	if len(got) != len(prefix)+idDigits {
		t.Errorf("%q has %d characters, want %d", got, len(got), len(prefix)+idDigits)
	}
	if !alphanumeric.MatchString(got) || !validISOID(got) {
		t.Errorf("%q is not upper case letters and digits of ISO 20022", got)
	}
}

// TestAnEndToEndIDIsISO20022 (FR-30): an end-to-end id is the letters and digits of its prefix, up to 10, in
// upper case, and then the id in 25 base 36 digits: the same for the same id and different for another.
func TestAnEndToEndIDIsISO20022(t *testing.T) {
	id := ids.New()
	cases := map[string]string{
		"ALUNA":              "ALUNA",
		"aluna":              "ALUNA",
		"al-u_na ñ":          "ALUNA",
		"":                   "",
		"ABCDEFGHIJKLMNOPQR": "ABCDEFGHIJ",
	}
	for prefix, want := range cases {
		got := NewEndToEndID(prefix, id)
		expectEndToEndID(t, got, want)
		if got != NewEndToEndID(prefix, id) {
			t.Errorf("NewEndToEndID(%q) is not stable", prefix)
		}
	}
	if NewEndToEndID("ALUNA", id) == NewEndToEndID("ALUNA", ids.New()) {
		t.Fatal("two ids gave the same end-to-end id")
	}
}

// TestTheEndToEndIDOfKnownIDs: the zero id is 25 zeros and the largest, all ones, fits in 35 with a prefix of
// 10; both in base 36.
func TestTheEndToEndIDOfKnownIDs(t *testing.T) {
	cases := map[string]struct {
		id           ids.ID
		prefix, want string
	}{
		"zero":    {ids.ID{}, "ALUNA", "ALUNA" + strings.Repeat("0", 25)},
		"largest": {largestID(), "ABCDEFGHIJ", "ABCDEFGHIJ" + "F5LXX1ZZ5PNORYNQGLHZMSP33"},
	}
	for name, tc := range cases {
		got := NewEndToEndID(tc.prefix, tc.id)
		if got != tc.want {
			t.Errorf("%s = %q, want %q", name, got, tc.want)
		}
		expectEndToEndID(t, got, tc.prefix)
	}
}
