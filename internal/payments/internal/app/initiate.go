package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/payments/internal/domain"
	"github.com/DrewGGM/aluna/internal/pricing"
)

// Prefixes of an order's ISO 20022 identifiers (domain.NewEndToEndID).
const (
	endToEndPrefix    = "ALUNA"
	instructionPrefix = "INSTR"
	transactionPrefix = "TX"
)

// RailSimBreB is the simulated Bre-B rail (AR-2), the only one until the real one can be reached (D-1).
const RailSimBreB = "simbreb"

// ErrDuplicatePayment means the entity already made a payment with that reference.
var ErrDuplicatePayment = errors.New("a payment with that reference was already made")

// Order is a payment order as payments keeps it: the domain's order, its beneficiary, the caller's reference to
// it, and, once it is held, the hold on its account, what it holds, and the charges it covers, which are posted
// when it settles.
type Order struct {
	domain.Order
	Creditor    AccountRef
	ExternalRef string
	HoldID      ids.ID
	Held        money.Amount
	Fees        []pricing.Fee
	GMF         money.Amount
}

// InitiateInput is an outgoing payment of Amount from deposit account AccountID of LegalEntityID to Creditor,
// through Rail, priced by QuoteID if it names one and as it is made otherwise. ExternalRef is the caller's
// reference to it, one payment per reference.
type InitiateInput struct {
	LegalEntityID, AccountID, QuoteID ids.ID
	Amount                            money.Amount
	Rail, ExternalRef                 string
	Creditor                          AccountRef
}

// Initiation is what initiating a payment needs, within its transaction.
type Initiation interface {
	DepositAccount(ctx context.Context, entity, id ids.ID) (deposits.Account, error)
	// ScreenBeneficiary reports whether screening cleared o's creditor.
	ScreenBeneficiary(ctx context.Context, o Order) (cleared bool, err error)
	// Charges locks a, which must still be active, and are the fees and GMF of a transfer of amount from it, by
	// its quote if quoteID names one.
	Charges(ctx context.Context, a deposits.Account, amount money.Amount, quoteID ids.ID) (pricing.Charges, error)
	// Hold holds amount on a, already locked, against the clearing account of rail, and returns the hold.
	Hold(ctx context.Context, a deposits.Account, rail string, amount money.Amount) (ids.ID, error)
	// Record stores o (ErrDuplicatePayment if its reference was used), publishes it and, if it is held,
	// queues it to be sent.
	Record(ctx context.Context, o Order) error
}

// Initiate starts an outgoing payment (FR-31) from an active account of the entity, in its currency, to a
// beneficiary by account or by key, through a known rail. Screening decides (FR-42): if it does not clear the
// beneficiary, the order is pending_screening and nothing moves; if it does, its amount and its charges are
// held on the account and the order is held, to be sent. The account is locked only after screening, so that a
// slow vendor does not keep its other movements waiting, and before the charges, in the order deposits locks;
// Charges checks again that it is active.
// Without funds for all of it, it is ledger.ErrInsufficientFunds.
func Initiate(ctx context.Context, p Initiation, in InitiateInput) (Order, error) {
	a, err := p.DepositAccount(ctx, in.LegalEntityID, in.AccountID)
	if err != nil {
		return Order{}, err
	}
	o, err := newOrder(a, in)
	if err != nil {
		return Order{}, err
	}
	cleared, err := p.ScreenBeneficiary(ctx, o)
	if err != nil {
		return Order{}, err
	}
	if cleared {
		err = hold(ctx, p, a, &o, in.QuoteID)
	} else {
		err = o.AwaitCompliance()
	}
	if err != nil {
		return Order{}, err
	}
	if err := p.Record(ctx, o); err != nil {
		return Order{}, err
	}
	return o, nil
}

// Longest beneficiary account number and key (payments.orders).
const (
	maxCreditorNumber = 64
	maxCreditorKey    = 128
)

// newOrder is in's order from a, being screened.
func newOrder(a deposits.Account, in InitiateInput) (Order, error) {
	switch {
	case a.Status != deposits.Active:
		return Order{}, fmt.Errorf("%w: account %s is %s", deposits.ErrAccountNotActive, a.ID, a.Status)
	case in.Rail != RailSimBreB:
		return Order{}, fmt.Errorf("%w: unknown rail %q", domain.ErrInvalidOrder, in.Rail)
	case in.ExternalRef == "":
		return Order{}, fmt.Errorf("%w: missing reference", domain.ErrInvalidOrder)
	case in.Amount.Currency() != a.Currency:
		return Order{}, fmt.Errorf("%w: the payment is in %s and the account in %s", domain.ErrInvalidOrder, in.Amount.Currency().Code(),
			a.Currency.Code())
	case (in.Creditor.Number == "") == (in.Creditor.Key == ""):
		return Order{}, fmt.Errorf("%w: the beneficiary goes by account or by key", domain.ErrInvalidOrder)
	case !validText(in.Creditor.Number, maxCreditorNumber) || !validText(in.Creditor.Key, maxCreditorKey):
		return Order{}, fmt.Errorf("%w: the beneficiary's account is up to %d characters and its key up to %d, of text",
			domain.ErrInvalidOrder, maxCreditorNumber, maxCreditorKey)
	}
	id := ids.New()
	o, err := domain.NewOutgoing(domain.OutgoingInput{
		ID: id, LegalEntityID: a.LegalEntityID, AccountID: a.ID, Amount: in.Amount, Rail: in.Rail,
		EndToEndID: domain.NewEndToEndID(endToEndPrefix, id), InstrID: domain.NewEndToEndID(instructionPrefix, id),
		TxID: domain.NewEndToEndID(transactionPrefix, id),
	})
	if err != nil {
		return Order{}, err
	}
	zero := money.New(0, in.Amount.Currency())
	order := Order{Order: o, Creditor: in.Creditor, ExternalRef: in.ExternalRef, Held: zero, GMF: zero}
	return order, order.StartScreening()
}

// validText reports whether s is valid UTF-8 of up to most characters without NUL, which PostgreSQL does not
// take.
func validText(s string, most int) bool {
	return utf8.ValidString(s) && utf8.RuneCountInString(s) <= most && !strings.ContainsRune(s, 0)
}

// hold holds o's amount and its charges on a and moves o to held.
func hold(ctx context.Context, p Initiation, a deposits.Account, o *Order, quoteID ids.ID) error {
	charges, err := p.Charges(ctx, a, o.Amount, quoteID)
	if err != nil {
		return err
	}
	fees, err := pricing.Total(charges.Fees, o.Amount.Currency())
	if err != nil {
		return err
	}
	total, err := o.Amount.Add(fees)
	if err != nil {
		return err
	}
	if total, err = total.Add(charges.GMF); err != nil {
		return err
	}
	if o.HoldID, err = p.Hold(ctx, a, o.Rail, total); err != nil {
		return err
	}
	o.Held, o.Fees, o.GMF = total, charges.Fees, charges.GMF
	return o.Hold()
}
