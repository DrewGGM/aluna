package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/deposits/internal/domain"
	"github.com/DrewGGM/aluna/internal/deposits/internal/domain/postingrules"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/pricing"
)

var (
	// ErrAccountNotActive means the account is not active: only an active account moves money (FR-22).
	ErrAccountNotActive = errors.New("the deposit account is not active")
	// ErrProductAccountMissing means the entity has no ledger account with the code the product names, in the
	// deposit account's currency: the product is misconfigured, not the request.
	ErrProductAccountMissing = errors.New("the product's ledger account does not exist")
	// ErrMinimumWageMissing means a product with limits in SMMLV and no minimum wage in force on the business
	// date: the market parameters are missing, not the request.
	ErrMinimumWageMissing = errors.New("no minimum wage (SMMLV) is in force on the business date")
)

// Source of the deposit movements in the ledger and their transaction codes.
const (
	movementSource = "deposits"
	codeDeposit    = "deposits.cash_deposit"
	codeWithdrawal = "deposits.withdrawal"
	codeTransfer   = "deposits.internal_transfer"
)

// CashInput is cash into or out of deposit account AccountID of the entity. ExternalRef identifies the
// movement among the entity's deposit movements: a retry with the same one is ledger.ErrDuplicateExternalRef
// and posts nothing. Actor (a subject) and Channel are recorded with the transaction. QuoteID, of a
// withdrawal only, is the quote that priced it; the zero ID prices it as it is done.
type CashInput struct {
	LegalEntityID, AccountID, QuoteID ids.ID
	Amount                            money.Amount
	ExternalRef                       string
	Actor, Channel                    string
}

// TransferInput moves money from deposit account From of the entity to its account To; ExternalRef, Actor,
// Channel and QuoteID as in CashInput.
type TransferInput struct {
	LegalEntityID, From, To, QuoteID ids.ID
	Amount                           money.Amount
	ExternalRef                      string
	Actor, Channel                   string
}

// header is what a movement's transaction records besides its entries.
type header struct{ code, externalRef, actor, channel string }

// side says whether a movement takes money out of an account (a debit) or puts it in (a credit).
type side int

const (
	debit side = iota + 1
	credit
)

// leg is an account a movement debits or credits; its limits and counters are its own.
type leg struct {
	account domain.Account
	side    side
}

// moved are the debits and credits of units on the leg.
func (g leg) moved(units int64) (debits, credits int64) {
	if g.side == credit {
		return 0, units
	}
	return units, 0
}

// movement is what move posts: its transaction and the legs it moves, the customer's account it is done on
// first.
type movement struct {
	header
	units   int64
	entries []ledger.EntryInput
	legs    []leg
}

// Movements is what moving an account's money needs, within the transaction it runs in.
type Movements interface {
	// LockAccount reads account id of the entity and keeps it locked (FOR UPDATE) until the transaction
	// ends.
	LockAccount(ctx context.Context, entity, id ids.ID) (domain.Account, error)
	// CashAccount is the ledger account the product of a names for its cash, or ErrProductAccountMissing.
	CashAccount(ctx context.Context, a domain.Account) (ids.ID, error)
	BusinessDate(ctx context.Context, entity ids.ID) (calendar.Date, error)
	Post(ctx context.Context, in ledger.PostInput) (ledger.Transaction, error)
	// RecordActivity sets the account's last customer activity to on.
	RecordActivity(ctx context.Context, id ids.ID, on calendar.Date) error
}

// Limiting is what keeping a movement within the product's limits needs (FR-24), within the transaction it
// runs in.
type Limiting interface {
	// Limits are the limits of the product of versionID.
	Limits(ctx context.Context, versionID ids.ID) (domain.Limits, error)
	// SMMLV is the minimum wage in pesos in force on day on, or ErrMinimumWageMissing.
	SMMLV(ctx context.Context, on calendar.Date) (money.Precise, error)
	// Used is what account a used of its limits before a movement on day on.
	Used(ctx context.Context, a domain.Account, on calendar.Date) (domain.Usage, error)
	// Count adds a movement's debits and credits to the counters of account id of day on and its month.
	Count(ctx context.Context, id ids.ID, on calendar.Date, debits, credits int64) error
}

// Charging is what charging an operation's fees and GMF needs (FR-28, FR-29), within the transaction it runs
// in.
type Charging interface {
	// Charges are what op of amount on account a pays: what quote quoteID quoted or, with the zero ID, what
	// it costs now.
	Charges(ctx context.Context, a domain.Account, op pricing.Operation, amount money.Amount, quoteID ids.ID) (pricing.Charges, error)
	// FeeIncome is the ledger account the product of a names for its fees, or ErrProductAccountMissing.
	FeeIncome(ctx context.Context, a domain.Account) (ids.ID, error)
	// GMFPayable is the ledger account the product of a names for the GMF, or ErrProductAccountMissing.
	GMFPayable(ctx context.Context, a domain.Account) (ids.ID, error)
}

// cash is a kind of cash movement: the side of the account it moves, its transaction code, its posting rule
// and what it charges besides, given the account.
type cash struct {
	side    side
	code    string
	rule    func(postingrules.Movement) ([]ledger.EntryInput, error)
	charges func(domain.Account) ([]ledger.EntryInput, error)
}

// Deposit posts cash into an active account from the product's cash account (FR-23), within its limits; it
// charges nothing.
func Deposit(ctx context.Context, s Movements, l Limiting, in CashInput) (ledger.Transaction, error) {
	return cashMovement(ctx, s, l, in, cash{
		side: credit, code: codeDeposit, rule: postingrules.CashDeposit,
		charges: func(domain.Account) ([]ledger.EntryInput, error) { return []ledger.EntryInput{}, nil },
	})
}

// Withdraw posts cash out of an active account to the product's cash account (FR-23), within its limits, with
// its fees and its GMF in the same transaction (FR-28, FR-29); the ledger rejects it without funds for all of
// them (ledger.ErrInsufficientFunds).
func Withdraw(ctx context.Context, s Movements, l Limiting, c Charging, in CashInput) (ledger.Transaction, error) {
	return cashMovement(ctx, s, l, in, cash{
		side: debit, code: codeWithdrawal, rule: postingrules.Withdrawal,
		charges: func(a domain.Account) ([]ledger.EntryInput, error) {
			return charged(ctx, c, a, pricing.Withdrawal, in.Amount, in.QuoteID)
		},
	})
}

// cashMovement posts in as k on its account, locked and active, with what k charges.
func cashMovement(ctx context.Context, s Movements, l Limiting, in CashInput, k cash) (ledger.Transaction, error) {
	a, err := lockActive(ctx, s, in.LegalEntityID, in.AccountID)
	if err != nil {
		return ledger.Transaction{}, err
	}
	vault, err := s.CashAccount(ctx, a)
	if err != nil {
		return ledger.Transaction{}, err
	}
	entries, err := k.rule(postingrules.Movement{Amount: in.Amount, Account: a.LedgerAccountID, Counter: vault})
	if err != nil {
		return ledger.Transaction{}, err
	}
	charges, err := k.charges(a)
	if err != nil {
		return ledger.Transaction{}, err
	}
	return move(ctx, s, l, movement{
		header: header{k.code, in.ExternalRef, in.Actor, in.Channel}, units: in.Amount.Units(), entries: append(entries, charges...),
		legs: []leg{{a, k.side}},
	})
}

// Transfer moves money between two active accounts of the entity (FR-23, US-5), within the limits of both:
// a debit of From and a credit of To, with From's fees and GMF in the same transaction (FR-28, FR-29). The
// ledger rejects it without funds in From for all of them. Only From records customer activity: what To
// receives is not its customer's doing.
func Transfer(ctx context.Context, s Movements, l Limiting, c Charging, in TransferInput) (ledger.Transaction, error) {
	from, to, err := lockPair(ctx, s, in.LegalEntityID, in.From, in.To)
	if err != nil {
		return ledger.Transaction{}, err
	}
	entries, err := postingrules.InternalTransfer(postingrules.Movement{Amount: in.Amount, Account: from.LedgerAccountID, Counter: to.LedgerAccountID})
	if err != nil {
		return ledger.Transaction{}, err
	}
	charges, err := charged(ctx, c, from, pricing.Transfer, in.Amount, in.QuoteID)
	if err != nil {
		return ledger.Transaction{}, err
	}
	return move(ctx, s, l, movement{
		header: header{codeTransfer, in.ExternalRef, in.Actor, in.Channel}, units: in.Amount.Units(), entries: append(entries, charges...),
		legs: []leg{{from, debit}, {to, credit}},
	})
}

// charged are the entries of what op of amount on account a pays besides the movement (FR-28, FR-29): its
// fees and its GMF. A quote's rejection (pricing.ErrQuoteUnavailable) thus comes before the ledger's.
func charged(ctx context.Context, c Charging, a domain.Account, op pricing.Operation, amount money.Amount, quoteID ids.ID,
) ([]ledger.EntryInput, error) {
	if amount.Currency() != a.Currency {
		return []ledger.EntryInput{}, nil // nothing to price: the ledger rejects the movement (ledger.ErrCurrencyMismatch)
	}
	charges, err := c.Charges(ctx, a, op, amount, quoteID)
	if err != nil {
		return nil, err
	}
	fees, err := feeEntries(ctx, c, a, charges.Fees)
	if err != nil {
		return nil, err
	}
	gmf, err := gmfEntries(ctx, c, a, charges.GMF)
	if err != nil {
		return nil, err
	}
	return append(fees, gmf...), nil
}

// feeEntries charge fees to account a, to its product's fee income account, which an operation without fees
// does not look up.
func feeEntries(ctx context.Context, c Charging, a domain.Account, fees []pricing.Fee) ([]ledger.EntryInput, error) {
	if len(fees) == 0 {
		return []ledger.EntryInput{}, nil
	}
	income, err := c.FeeIncome(ctx, a)
	if err != nil {
		return nil, err
	}
	var entries []ledger.EntryInput
	for _, f := range fees {
		fee, err := postingrules.FeeCharge(postingrules.Movement{Amount: f.Amount, Account: a.LedgerAccountID, Counter: income})
		if err != nil {
			return nil, err
		}
		entries = append(entries, fee...)
	}
	return entries, nil
}

// gmfEntries withhold gmf from account a, to its product's GMF payable account; a GMF of zero is not posted,
// nor is that account looked up.
func gmfEntries(ctx context.Context, c Charging, a domain.Account, gmf money.Amount) ([]ledger.EntryInput, error) {
	if gmf.Sign() == 0 {
		return []ledger.EntryInput{}, nil
	}
	payable, err := c.GMFPayable(ctx, a)
	if err != nil {
		return nil, err
	}
	return postingrules.GMFWithholding(postingrules.Movement{Amount: gmf, Account: a.LedgerAccountID, Counter: payable})
}

// move posts m on the entity's business date within the limits of each of its legs (FR-24) and adds it to
// their counters. What the legs used is read before posting, under their locks, so nothing changes it in
// between; the limits are checked after the ledger accepts the posting, so that its rejections (a reference
// already posted, another currency, no funds) come first, after those of the charges' quote. A limit exceeded
// fails the movement after posting, so whoever runs it undoes what it wrote (the facade, under a savepoint):
// nothing is credited in part.
func move(ctx context.Context, s Movements, l Limiting, m movement) (ledger.Transaction, error) {
	today, err := s.BusinessDate(ctx, m.legs[0].account.LegalEntityID)
	if err != nil {
		return ledger.Transaction{}, err
	}
	limits := make([]limitsOn, len(m.legs))
	for i, g := range m.legs {
		if limits[i], err = limitsOf(ctx, l, g.account, today); err != nil {
			return ledger.Transaction{}, err
		}
	}
	posted, err := post(ctx, s, m.legs[0].account, today, m.header, m.entries)
	if err != nil {
		return ledger.Transaction{}, err
	}
	for i, g := range m.legs {
		if err := limits[i].check(g.side, m.units); err != nil {
			return ledger.Transaction{}, err
		}
	}
	for _, g := range m.legs {
		debits, credits := g.moved(m.units)
		if err := l.Count(ctx, g.account.ID, today, debits, credits); err != nil {
			return ledger.Transaction{}, err
		}
	}
	return posted, nil
}

// limitsOn are an account's limits on a day and what it used of them before a movement.
type limitsOn struct {
	caps domain.Caps
	used domain.Usage
}

// limitsOf reads account a's limits on day on and, if any applies, what it used of them.
func limitsOf(ctx context.Context, l Limiting, a domain.Account, on calendar.Date) (limitsOn, error) {
	limits, err := l.Limits(ctx, a.ProductVersionID)
	if err != nil {
		return limitsOn{}, err
	}
	var wage money.Precise
	if limits.UsesSMMLV() {
		if wage, err = l.SMMLV(ctx, on); err != nil {
			return limitsOn{}, err
		}
	}
	caps, err := limits.InUnits(a.Currency, wage)
	if err != nil || caps.Unlimited() {
		return limitsOn{caps: caps}, err
	}
	used, err := l.Used(ctx, a, on)
	if err != nil {
		return limitsOn{}, err
	}
	return limitsOn{caps: caps, used: used}, nil
}

// check is a *domain.LimitError if a movement of units on the side sd exceeds a limit.
func (o limitsOn) check(sd side, units int64) error {
	if sd == debit {
		return o.caps.CheckDebit(o.used, units)
	}
	return o.caps.CheckCredit(o.used, units)
}

// lockPair locks accounts from and to of the entity, both active, in id order: two opposite transfers do not
// deadlock. The same account twice is locked twice, which PostgreSQL allows.
func lockPair(ctx context.Context, s Movements, entity, from, to ids.ID) (domain.Account, domain.Account, error) {
	swapped := bytes.Compare(from[:], to[:]) > 0
	if swapped {
		from, to = to, from
	}
	first, err := lockActive(ctx, s, entity, from)
	if err != nil {
		return domain.Account{}, domain.Account{}, err
	}
	second, err := lockActive(ctx, s, entity, to)
	if err != nil {
		return domain.Account{}, domain.Account{}, err
	}
	if swapped {
		return second, first, nil
	}
	return first, second, nil
}

// lockActive locks account id of the entity and checks, under the lock, that it is active: the contract of
// ChangeStatus.
func lockActive(ctx context.Context, s Movements, entity, id ids.ID) (domain.Account, error) {
	a, err := s.LockAccount(ctx, entity, id)
	if err != nil {
		return domain.Account{}, err
	}
	if a.Status != domain.Active {
		return domain.Account{}, fmt.Errorf("%w: %s is %s", ErrAccountNotActive, a.ID, a.Status)
	}
	return a, nil
}

// post posts entries on today, the entity's business date, as value date too, and records it as the last
// activity of customer account a.
func post(ctx context.Context, s Movements, a domain.Account, today calendar.Date, h header, entries []ledger.EntryInput) (ledger.Transaction, error) {
	day := dateTime(today)
	posted, err := s.Post(ctx, ledger.PostInput{
		LegalEntityID: a.LegalEntityID, Code: h.code, ValueDate: day, BusinessDate: day,
		Source: movementSource, ExternalRef: h.externalRef, Actor: h.actor, Channel: h.channel,
		RuleVersion: postingrules.RuleVersion, Entries: entries,
	})
	if err != nil {
		return ledger.Transaction{}, err
	}
	if a.LastCustomerActivityOn != today {
		if err := s.RecordActivity(ctx, a.ID, today); err != nil {
			return ledger.Transaction{}, err
		}
	}
	return posted, nil
}
