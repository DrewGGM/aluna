// Package domain models payment orders (FR-30): an order with its ISO 20022 identifiers and the state machine
// an outgoing payment goes through, from screening to its settlement, rejection or return, and the one of a
// transfer received, credited or held in suspense (FR-32).
package domain

import (
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strings"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
)

// Direction is which way a payment goes.
type Direction string

// Directions.
const (
	Outgoing Direction = "outgoing"
	Incoming Direction = "incoming"
)

// Status is where an order stands (architecture §5, outgoing payment order).
type Status string

// Statuses. An outgoing order ends settled, rejected or returned; an incoming one, settled (credited) or
// suspended.
const (
	StatusInitiated        Status = "initiated"
	StatusScreening        Status = "screening"
	StatusPendingScreening Status = "pending_screening" // a possible match or no answer: compliance decides
	StatusHeld             Status = "held"              // screening let it through: its funds are held, or the ledger refuses them
	StatusSent             Status = "sent"              // handed to the rail
	StatusSettled          Status = "settled"
	StatusRejected         Status = "rejected"
	StatusReturned         Status = "returned"  // settled and then returned by the beneficiary's bank
	StatusSuspended        Status = "suspended" // received for no account it can credit: held in the rail's suspense account
)

var (
	// ErrInvalidOrder means the order's data is invalid.
	ErrInvalidOrder = errors.New("invalid payment order")
	// ErrIllegalTransition means the order cannot move that way from where it stands.
	ErrIllegalTransition = errors.New("illegal payment order transition")
)

// Order is a payment of Amount from or to AccountID, a deposit account of LegalEntityID, through Rail; an
// incoming one for no account the core can credit has none. EndToEndID, InstrID and TxID are its ISO 20022
// identifiers, and DebtorAgent, of an incoming one, the bank that sent it, whose EndToEndID it is. Reason is why
// it was rejected or suspended, if it was.
type Order struct {
	ID, LegalEntityID, AccountID ids.ID
	Direction                    Direction
	Amount                       money.Amount
	Rail                         string
	EndToEndID, InstrID, TxID    string
	DebtorAgent                  string
	Status                       Status
	Reason                       string
}

// OutgoingInput is a new outgoing payment.
type OutgoingInput struct {
	ID, LegalEntityID, AccountID ids.ID
	Amount                       money.Amount
	Rail                         string
	EndToEndID, InstrID, TxID    string
}

// NewOutgoing is a new outgoing order, initiated: it has its own id, its entity's and its account's, its
// amount is positive, it names its rail, and its identifiers are ISO 20022 (validISOID).
func NewOutgoing(in OutgoingInput) (Order, error) {
	switch {
	case in.ID == ids.ID{} || in.LegalEntityID == ids.ID{} || in.AccountID == ids.ID{}:
		return Order{}, fmt.Errorf("%w: missing the order, its entity or its account", ErrInvalidOrder)
	case in.Amount.Sign() <= 0:
		return Order{}, fmt.Errorf("%w: the amount must be positive", ErrInvalidOrder)
	case in.Rail == "":
		return Order{}, fmt.Errorf("%w: missing rail", ErrInvalidOrder)
	}
	if err := checkISOIDs(isoField{"EndToEndId", in.EndToEndID}, isoField{"InstrId", in.InstrID}, isoField{"TxId", in.TxID}); err != nil {
		return Order{}, err
	}
	return Order{
		ID: in.ID, LegalEntityID: in.LegalEntityID, AccountID: in.AccountID, Direction: Outgoing, Amount: in.Amount,
		Rail: in.Rail, EndToEndID: in.EndToEndID, InstrID: in.InstrID, TxID: in.TxID, Status: StatusInitiated,
	}, nil
}

// IncomingInput is a transfer received from DebtorAgent, the bank that sent it, for an account of LegalEntityID.
type IncomingInput struct {
	ID, LegalEntityID         ids.ID
	Amount                    money.Amount
	Rail                      string
	EndToEndID, InstrID, TxID string
	DebtorAgent               string
}

// agentCode is a bank's identifier as a rail names it, a BIC or a participant code: letters and digits only,
// so that it and an end-to-end id joined by a slash are one pair.
var agentCode = regexp.MustCompile(`^[0-9A-Za-z]{1,35}$`)

// NewIncoming is a new incoming order, initiated and without an account until its creditor is resolved: it has
// its own id and its entity's, its amount is positive, it names its rail, its identifiers are ISO 20022
// (validISOID) and its debtor agent is a bank's code (agentCode).
func NewIncoming(in IncomingInput) (Order, error) {
	switch {
	case in.ID == ids.ID{} || in.LegalEntityID == ids.ID{}:
		return Order{}, fmt.Errorf("%w: missing the order or its entity", ErrInvalidOrder)
	case in.Amount.Sign() <= 0:
		return Order{}, fmt.Errorf("%w: the amount must be positive", ErrInvalidOrder)
	case in.Rail == "":
		return Order{}, fmt.Errorf("%w: missing rail", ErrInvalidOrder)
	}
	if err := checkISOIDs(isoField{"EndToEndId", in.EndToEndID}, isoField{"InstrId", in.InstrID}, isoField{"TxId", in.TxID}); err != nil {
		return Order{}, err
	}
	if !agentCode.MatchString(in.DebtorAgent) {
		return Order{}, fmt.Errorf("%w: DbtrAgt must be 1 to 35 letters and digits", ErrInvalidOrder)
	}
	return Order{
		ID: in.ID, LegalEntityID: in.LegalEntityID, Direction: Incoming, Amount: in.Amount, Rail: in.Rail, EndToEndID: in.EndToEndID,
		InstrID: in.InstrID, TxID: in.TxID, DebtorAgent: in.DebtorAgent, Status: StatusInitiated,
	}, nil
}

// isoField is an identifier of an order and its ISO 20022 name.
type isoField struct{ name, value string }

// checkISOIDs is ErrInvalidOrder, naming the first, if one of fields is not an ISO 20022 identifier.
func checkISOIDs(fields ...isoField) error {
	for _, f := range fields {
		if !validISOID(f.value) {
			return fmt.Errorf("%w: %s must be 1 to 35 characters of the ISO 20022 restricted set, without a leading, trailing or double slash",
				ErrInvalidOrder, f.name)
		}
	}
	return nil
}

// StartScreening moves an initiated outgoing order to screening.
func (o *Order) StartScreening() error { return o.move(Outgoing, StatusScreening, StatusInitiated) }

// AwaitCompliance moves an order being screened to pending_screening: screening did not clear it.
func (o *Order) AwaitCompliance() error {
	return o.move(Outgoing, StatusPendingScreening, StatusScreening)
}

// Hold moves to held an order that screening cleared, or compliance released: its funds are held next, and
// if the ledger refuses them the order is rejected from held.
func (o *Order) Hold() error {
	return o.move(Outgoing, StatusHeld, StatusScreening, StatusPendingScreening)
}

// Send moves a held order to sent: it was handed to the rail.
func (o *Order) Send() error { return o.move(Outgoing, StatusSent, StatusHeld) }

// Settle moves a sent order to settled: the rail confirmed it.
func (o *Order) Settle() error { return o.move(Outgoing, StatusSettled, StatusSent) }

// Credit moves an initiated incoming order to settled: it credits its account.
func (o *Order) Credit() error { return o.move(Incoming, StatusSettled, StatusInitiated) }

// Suspend moves an initiated incoming order to suspended, for reason: it credits the rail's suspense account,
// for no account of the entity can take it. A suspense says why: without a reason it is ErrInvalidOrder.
func (o *Order) Suspend(reason string) error {
	if reason == "" {
		return fmt.Errorf("%w: a suspense needs its reason", ErrInvalidOrder)
	}
	if err := o.move(Incoming, StatusSuspended, StatusInitiated); err != nil {
		return err
	}
	o.Reason = reason
	return nil
}

// Reject moves to rejected, for reason, an order that compliance confirmed as a match, whose funds the ledger
// would not hold (no funds, a limit) or that the rail rejected. A rejection says why: without a reason it is
// ErrInvalidOrder.
func (o *Order) Reject(reason string) error {
	if reason == "" {
		return fmt.Errorf("%w: a rejection needs its reason", ErrInvalidOrder)
	}
	if err := o.move(Outgoing, StatusRejected, StatusPendingScreening, StatusHeld, StatusSent); err != nil {
		return err
	}
	o.Reason = reason
	return nil
}

// Return moves a settled order to returned: the beneficiary's bank sent it back.
func (o *Order) Return() error { return o.move(Outgoing, StatusReturned, StatusSettled) }

// move takes an order of direction d to status to if it stands in one of from, and is ErrIllegalTransition
// otherwise.
func (o *Order) move(d Direction, to Status, from ...Status) error {
	if o.Direction != d || !slices.Contains(from, o.Status) {
		return fmt.Errorf("%w: %s %s to %s", ErrIllegalTransition, o.Direction, o.Status, to)
	}
	o.Status = to
	return nil
}

// isoID is ISO 20022 RestrictedFINXMax35Text, the set Bre-B takes for identifiers.
var isoID = regexp.MustCompile(`^[0-9A-Za-z/\-?:().,'+ ]{1,35}$`)

// validISOID reports whether id is an ISO 20022 identifier: 1 to 35 characters of the restricted set that
// neither start nor end with a slash nor have two in a row.
func validISOID(id string) bool {
	return isoID.MatchString(id) && !strings.HasPrefix(id, "/") && !strings.HasSuffix(id, "/") && !strings.Contains(id, "//")
}

const (
	maxPrefix = 10 // so that the prefix and the id fit in 35
	idDigits  = 25 // base 36 digits of the largest 128-bit id
)

// NewEndToEndID is the ISO 20022 EndToEndId of order id: the letters and digits of prefix, up to 10, followed
// by id in 25 base 36 digits, all in upper case (a rail may compare identifiers without case). It is at most
// 35 characters, the same for the same id, and different for another.
func NewEndToEndID(prefix string, id ids.ID) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(prefix) {
		if b.Len() < maxPrefix && (r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	digits := strings.ToUpper(new(big.Int).SetBytes(id[:]).Text(36))
	return b.String() + strings.Repeat("0", idDigits-len(digits)) + digits
}
