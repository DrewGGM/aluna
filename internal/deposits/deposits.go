// Package deposits is the deposits module: deposit accounts, their life cycle, their movements and, in their
// own files, their interest (FR-22 to FR-27).
package deposits

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/DrewGGM/aluna/internal/calendar"
	pg "github.com/DrewGGM/aluna/internal/deposits/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/deposits/internal/app"
	"github.com/DrewGGM/aluna/internal/deposits/internal/domain"
	"github.com/DrewGGM/aluna/internal/deposits/internal/domain/postingrules"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/party"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/products"
)

type (
	// Account is a deposit account.
	Account = domain.Account
	// Status is the status of a deposit account.
	Status = domain.Status
	// OpenInput is a deposit account to open.
	OpenInput = app.OpenInput
	// CashInput is cash into or out of a deposit account.
	CashInput = app.CashInput
	// TransferInput moves money between two deposit accounts of an entity.
	TransferInput = app.TransferInput
	// LimitError names the limit a movement exceeds.
	LimitError = domain.LimitError
)

// Statuses of a deposit account.
const (
	Pending = domain.Pending
	Active  = domain.Active
	Frozen  = domain.Frozen
	Dormant = domain.Dormant
	Closed  = domain.Closed
)

var (
	// ErrKYCNotSatisfied means the party's KYC status is not one the product allows (FR-17).
	ErrKYCNotSatisfied = app.ErrKYCNotSatisfied
	// ErrAccountNotFound means the deposit account does not exist.
	ErrAccountNotFound = app.ErrAccountNotFound
	// ErrInvalidTransition means the account cannot go from its status to the requested one.
	ErrInvalidTransition = domain.ErrInvalidTransition
	// ErrNotEmpty means an account with a balance cannot close.
	ErrNotEmpty = domain.ErrNotEmpty
	// ErrPendingHolds means an account with pending holds cannot close.
	ErrPendingHolds = domain.ErrPendingHolds
	// ErrAccountNotActive means the account is not active: only an active account moves money.
	ErrAccountNotActive = app.ErrAccountNotActive
	// ErrInvalidMovement means an amount that is not positive or a transfer to the same account.
	ErrInvalidMovement = postingrules.ErrInvalidMovement
	// ErrProductAccountMissing means the entity has no ledger account with the code the product names, in the
	// account's currency.
	ErrProductAccountMissing = app.ErrProductAccountMissing
	// ErrLimitExceeded means a movement over a limit of the product (FR-24); the error is a *LimitError.
	ErrLimitExceeded = domain.ErrLimitExceeded
	// ErrMinimumWageMissing means a product with limits in SMMLV and no minimum wage in force.
	ErrMinimumWageMissing = app.ErrMinimumWageMissing
	// ErrSMMLVOutsideCOP means a product with a limit in SMMLV on an account that is not in COP.
	ErrSMMLVOutsideCOP = domain.ErrSMMLVOutsideCOP
	// ErrMinimumWageTooLow means a minimum wage that makes a limit in SMMLV less than one minor unit.
	ErrMinimumWageTooLow = domain.ErrMinimumWageTooLow
)

// typeOpened is the event of an opened account (architecture §8).
const typeOpened = "aluna.deposits.account.opened.v1"

// Events publishes deposit events; outbox.Outbox implements it.
type Events interface {
	Emit(ctx context.Context, tx postgres.Tx, e outbox.Event) error
}

// Service manages deposit accounts with the modules they depend on.
type Service struct {
	ledger   *ledger.Service
	parties  *party.Service
	products *products.Service
	calendar *calendar.Service
	events   Events
}

// New creates the service.
func New(l *ledger.Service, parties *party.Service, p *products.Service, c *calendar.Service, events Events) *Service {
	return &Service{ledger: l, parties: parties, products: p, calendar: c, events: events}
}

// Open opens, within tx, a deposit account for a party of the entity with the version in force of a
// product of the entity, and emits aluna.deposits.account.opened.v1. A KYC the product does not allow is
// ErrKYCNotSatisfied; a product without a published version, products.ErrNoPublishedVersion; another
// entity's product, products.ErrProductNotFound; an unknown party, party.ErrNotFound.
func (s *Service) Open(ctx context.Context, tx postgres.Tx, in OpenInput) (Account, error) {
	a, err := app.Open(ctx, opening{Store: pg.NewStore(tx), s: s, tx: tx}, in)
	if err != nil {
		return Account{}, err
	}
	err = s.events.Emit(ctx, tx, outbox.Event{
		Type: typeOpened, Subject: "deposit-accounts/" + a.ID.String(), LegalEntityID: a.LegalEntityID, SchemaVersion: 1,
		Data: openedPayload{
			AccountID: a.ID, PartyID: a.PartyID, ProductVersionID: a.ProductVersionID, Currency: a.Currency.Code(), Status: a.Status,
		},
	})
	if err != nil {
		return Account{}, err
	}
	return a, nil
}

type openedPayload struct {
	AccountID        ids.ID `json:"account_id"`
	PartyID          ids.ID `json:"party_id"`
	ProductVersionID ids.ID `json:"product_version_id"`
	Currency         string `json:"currency"`
	Status           Status `json:"status"`
}

// ChangeStatus moves, within tx, account accountID of the entity to status to, recording actor (a subject)
// as who did it: ErrInvalidTransition if the diagram does not allow it, ErrKYCNotSatisfied to activate a
// pending account whose party's KYC is not approved, and closing needs no pending holds and a posted balance
// of 0 (ErrPendingHolds, ErrNotEmpty). Every movement of the account's money locks the account first (see
// app.ChangeStatus), or a close could leave it closed with a balance.
func (s *Service) ChangeStatus(ctx context.Context, tx postgres.Tx, entity, accountID ids.ID, to Status, actor string) error {
	return app.ChangeStatus(ctx, statusChanges{Store: pg.NewStore(tx), s: s, tx: tx}, entity, accountID, to, actor)
}

// Deposit posts, within tx, cash into an active account of the entity from its product's cash account and
// returns the ledger transaction. A rejection leaves nothing in tx: ErrAccountNotFound if the entity has no
// such account, ErrAccountNotActive if it is not active, ErrInvalidMovement if the amount is not positive,
// ledger.ErrCurrencyMismatch if it is not in the account's currency, ErrLimitExceeded (a *LimitError) if it
// exceeds a limit of the product (FR-24), ledger.ErrDuplicateExternalRef if in.ExternalRef was already posted,
// ErrProductAccountMissing if the entity lacks the cash account its product names, and ErrMinimumWageMissing,
// ErrMinimumWageTooLow or ErrSMMLVOutsideCOP if its limits in SMMLV cannot be converted (the minimum wage is
// not in force or is wrong, or the account is not in COP).
func (s *Service) Deposit(ctx context.Context, tx postgres.Tx, in CashInput) (ledger.Transaction, error) {
	m := s.movements(tx)
	return atomically(ctx, tx, func() (ledger.Transaction, error) { return app.Deposit(ctx, m, m, in) })
}

// Withdraw posts, within tx, cash out of an active account of the entity to its product's cash account, as
// Deposit; without funds it is ledger.ErrInsufficientFunds.
func (s *Service) Withdraw(ctx context.Context, tx postgres.Tx, in CashInput) (ledger.Transaction, error) {
	m := s.movements(tx)
	return atomically(ctx, tx, func() (ledger.Transaction, error) { return app.Withdraw(ctx, m, m, in) })
}

// Transfer moves, within tx, money between two active accounts of the entity, as Withdraw, within the limits
// of both; a transfer to the same account is ErrInvalidMovement.
func (s *Service) Transfer(ctx context.Context, tx postgres.Tx, in TransferInput) (ledger.Transaction, error) {
	m := s.movements(tx)
	return atomically(ctx, tx, func() (ledger.Transaction, error) { return app.Transfer(ctx, m, m, in) })
}

// undoTimeout bounds undoing a failed movement, which runs even if the request was canceled.
const undoTimeout = 5 * time.Second

// atomically runs move under a savepoint of tx and undoes it if move fails: a rejected movement leaves nothing
// in tx, as a rejected ledger posting does, although its limits are checked after posting.
func atomically(ctx context.Context, tx postgres.Tx, move func() (ledger.Transaction, error)) (ledger.Transaction, error) {
	if _, err := tx.Exec(ctx, "SAVEPOINT deposit_movement"); err != nil {
		return ledger.Transaction{}, fmt.Errorf("movement savepoint: %w", err)
	}
	posted, err := move()
	if err != nil {
		return ledger.Transaction{}, undo(ctx, tx, err)
	}
	if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT deposit_movement"); err != nil {
		return ledger.Transaction{}, fmt.Errorf("release the movement savepoint: %w", err)
	}
	return posted, nil
}

// undo rolls tx back to the movement's savepoint, which failed with err, and releases it, so many rejected
// movements in one transaction do not pile up savepoints. If it cannot, it returns that error and not err:
// the movement may still be in tx, so it must not pass for a clean rejection.
func undo(ctx context.Context, tx postgres.Tx, err error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), undoTimeout)
	defer cancel()
	if _, undoErr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT deposit_movement; RELEASE SAVEPOINT deposit_movement"); undoErr != nil {
		return fmt.Errorf("undo the movement that failed with %v: %w", err, undoErr) //nolint:errorlint // err must not match: the movement may not be undone
	}
	return err
}

// Account returns deposit account id of the entity, or ErrAccountNotFound. q can be the pool or a Tx.
func (s *Service) Account(ctx context.Context, q postgres.Querier, entity, id ids.ID) (Account, error) {
	return pg.NewStore(q).Account(ctx, entity, id)
}

// kycStatus is the current KYC status of party partyID of the entity, read once.
func (s *Service) kycStatus(ctx context.Context, tx postgres.Tx, entity, partyID ids.ID) (party.KYCStatus, error) {
	p, err := s.parties.Get(ctx, tx, entity, partyID)
	if err != nil {
		return "", err
	}
	return p.Current.KYCStatus, nil
}

// opening is app.Opening over tx; the store gives InsertAccount.
type opening struct {
	pg.Store
	s  *Service
	tx postgres.Tx
}

func (o opening) CurrentVersion(ctx context.Context, productID ids.ID) (products.Version, error) {
	return o.s.products.CurrentVersion(ctx, o.tx, productID)
}

func (o opening) KYCStatus(ctx context.Context, entity, partyID ids.ID) (party.KYCStatus, error) {
	return o.s.kycStatus(ctx, o.tx, entity, partyID)
}

func (o opening) BusinessDate(ctx context.Context, entity ids.ID) (calendar.Date, error) {
	return o.s.calendar.BusinessDate(ctx, o.tx, entity)
}

func (o opening) CreateLedgerAccount(ctx context.Context, in ledger.CreateAccountInput) (ledger.Account, error) {
	return o.s.ledger.CreateAccount(ctx, o.tx, in)
}

// statusChanges is app.StatusChanges over tx; the store gives LockAccount and SetStatus.
type statusChanges struct {
	pg.Store
	s  *Service
	tx postgres.Tx
}

func (c statusChanges) Balance(ctx context.Context, ledgerAccountID ids.ID) (ledger.Balance, error) {
	return c.s.ledger.Balance(ctx, c.tx, ledgerAccountID)
}

func (c statusChanges) KYCStatus(ctx context.Context, entity, partyID ids.ID) (party.KYCStatus, error) {
	return c.s.kycStatus(ctx, c.tx, entity, partyID)
}

func (s *Service) movements(tx postgres.Tx) movements {
	return movements{Store: pg.NewStore(tx), s: s, tx: tx, versions: map[ids.ID]products.Version{}}
}

// movements is app.Movements and app.Limiting over tx, for one movement; the store gives LockAccount,
// RecordActivity and Count. It reads each product version once: the cash account and the limits come from
// the same one.
type movements struct {
	pg.Store
	s        *Service
	tx       postgres.Tx
	versions map[ids.ID]products.Version
}

func (m movements) version(ctx context.Context, id ids.ID) (products.Version, error) {
	if v, ok := m.versions[id]; ok {
		return v, nil
	}
	v, err := m.s.products.Version(ctx, m.tx, id)
	if err != nil {
		return products.Version{}, err
	}
	m.versions[id] = v
	return v, nil
}

// CashAccount reads the product and the ledger in the adapter, which keeps the port at five methods: the
// cash account of the product of versionID, of the entity and in currency c.
func (m movements) CashAccount(ctx context.Context, entity, versionID ids.ID, c money.Currency) (ids.ID, error) {
	v, err := m.version(ctx, versionID)
	if err != nil {
		return ids.ID{}, err
	}
	code := v.Definition.Accounts.Cash
	a, err := m.s.ledger.AccountByCode(ctx, m.tx, entity, code)
	if errors.Is(err, ledger.ErrAccountNotFound) {
		return ids.ID{}, fmt.Errorf("%w: cash account %q of product version %s", ErrProductAccountMissing, code, versionID)
	}
	if err != nil {
		return ids.ID{}, err
	}
	if a.Currency != c {
		return ids.ID{}, fmt.Errorf("%w: cash account %q is in %s, not %s", ErrProductAccountMissing, code, a.Currency.Code(), c.Code())
	}
	return a.ID, nil
}

func (m movements) BusinessDate(ctx context.Context, entity ids.ID) (calendar.Date, error) {
	return m.s.calendar.BusinessDate(ctx, m.tx, entity)
}

func (m movements) Post(ctx context.Context, in ledger.PostInput) (ledger.Transaction, error) {
	return m.s.ledger.Post(ctx, m.tx, in)
}

// Limits maps the limits of the product's definition to the deposits domain, which does not import products.
func (m movements) Limits(ctx context.Context, versionID ids.ID) (domain.Limits, error) {
	v, err := m.version(ctx, versionID)
	if err != nil {
		return domain.Limits{}, err
	}
	l := v.Definition.Limits
	return domain.Limits{
		PerOperation:  domain.Limit{Units: l.PerOperation.Units, SMMLV: l.PerOperation.SMMLV},
		DailyDebits:   domain.Limit{Units: l.DailyDebits.Units, SMMLV: l.DailyDebits.SMMLV},
		MonthlyDebits: domain.Limit{Units: l.MonthlyDebits.Units, SMMLV: l.MonthlyDebits.SMMLV},
		MaxBalance:    domain.Limit{Units: l.MaxBalance.Units, SMMLV: l.MaxBalance.SMMLV},
	}, nil
}

// SMMLV is the minimum wage in force on day on; without one it is ErrMinimumWageMissing, which has no problem
// (a 500 the client can retry once it is loaded), and not products.ErrNoMarketParameter, which is a 404.
func (m movements) SMMLV(ctx context.Context, on calendar.Date) (money.Precise, error) {
	wage, err := m.s.products.MarketParameter(ctx, m.tx, products.SMMLV, "", on)
	if errors.Is(err, products.ErrNoMarketParameter) {
		return money.Precise{}, fmt.Errorf("%w: %d-%02d-%02d", ErrMinimumWageMissing, on.Year, on.Month, on.Day)
	}
	return wage, err
}

// Used combines the account's counters with its ledger balance: what it debited on day on and in its month,
// and what it holds (the natural balance of its liability ledger account).
func (m movements) Used(ctx context.Context, a domain.Account, on calendar.Date) (domain.Usage, error) {
	daily, monthly, err := m.Debits(ctx, a.ID, on)
	if err != nil {
		return domain.Usage{}, err
	}
	b, err := m.s.ledger.Balance(ctx, m.tx, a.LedgerAccountID)
	if err != nil {
		return domain.Usage{}, err
	}
	return domain.Usage{DailyDebits: daily, MonthlyDebits: monthly, Balance: ledger.NaturalBalance(ledger.KindLiability, b.Posted)}, nil
}
