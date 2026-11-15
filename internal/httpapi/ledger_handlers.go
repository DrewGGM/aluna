package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/DrewGGM/aluna/internal/access"
	"github.com/DrewGGM/aluna/internal/approvals"
	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/payments"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// manualRuleVersion is the rule_version of the postings of POST /v1/transactions, stored forever in
// ledger.transactions: their entries came built by the caller and not from a versioned posting rule.
const manualRuleVersion = "manual.v1"

// Maker-checker request kinds of a reversal (FR-3, FR-39) and of a manual posting over its threshold (ADR-0019), and
// the permissions required to approve them.
const (
	reversalKind            = "ledger.reversal"
	reversalPermission      = "ledger.transactions.reverse"
	manualPostingKind       = "ledger.manual_posting"
	manualPostingPermission = "ledger.transactions.post"
)

// LedgerHandlers implements the contract's ledger operations (api/openapi.yaml).
type LedgerHandlers struct {
	ledger           *ledger.Service
	deposits         *deposits.Service
	approvals        *approvals.Service
	commands         commands
	reader           postgres.Querier
	manualThresholds ManualPostingThresholds
}

// NewLedgerHandlers creates the ledger handlers and registers in a the request kinds of a reversal and of a manual
// posting that reaches its threshold in thresholds (none if it is empty), which go through maker-checker. A posting on
// the ledger account of a deposit account first locks it through d and is refused if it is not active. keys stores
// the responses of the commands with Idempotency-Key; reader serves the reads outside a transaction. The legal
// entity of each request is that of its token (FR-41).
func NewLedgerHandlers(l *ledger.Service, d *deposits.Service, a *approvals.Service, uow *postgres.UnitOfWork, keys *idempotency.Store,
	reader postgres.Querier, thresholds ManualPostingThresholds,
) *LedgerHandlers {
	a.RegisterKind(reversalKind, reversalPermission, reverseOnApproval(l, d))
	a.RegisterKind(manualPostingKind, manualPostingPermission, postOnApproval(l, d))
	return &LedgerHandlers{ledger: l, deposits: d, approvals: a, commands: commands{uow: uow, keys: keys}, reader: reader, manualThresholds: thresholds}
}

// CreateLedgerAccount creates an account of the token's entity.
func (h *LedgerHandlers) CreateLedgerAccount(ctx context.Context, req gen.CreateLedgerAccountRequestObject) (gen.CreateLedgerAccountResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	cur, err := money.CurrencyOf(req.Body.Currency)
	if err != nil {
		return nil, fmt.Errorf("account currency: %w", err)
	}
	in := ledger.CreateAccountInput{
		LegalEntityID: p.LegalEntityID, Code: req.Body.Code, Kind: ledger.Kind(req.Body.Kind), Currency: cur,
		Mode: ledger.BalanceMode(req.Body.BalanceMode),
	}
	if req.Body.LowerLimit != nil {
		in.LowerLimit = *req.Body.LowerLimit
	}
	resp, err := h.commands.run(ctx, createLedgerAccount, func(tx postgres.Tx) (any, error) {
		account, err := h.ledger.CreateAccount(ctx, tx, in)
		if err != nil {
			return nil, err
		}
		return toAccountJSON(account), nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// GetLedgerAccount reads an account of the token's entity.
func (h *LedgerHandlers) GetLedgerAccount(ctx context.Context, req gen.GetLedgerAccountRequestObject) (gen.GetLedgerAccountResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	account, err := h.ledger.Account(ctx, h.reader, p.LegalEntityID, ids.ID(req.Id))
	if err != nil {
		return nil, err
	}
	return gen.GetLedgerAccount200JSONResponse(toAccountJSON(account)), nil
}

// GetLedgerAccountBalance reads the balance of an account of the token's entity: the materialized one or,
// with a value date or knowledge cutoff, the bitemporal one.
func (h *LedgerHandlers) GetLedgerAccountBalance(ctx context.Context, req gen.GetLedgerAccountBalanceRequestObject) (gen.GetLedgerAccountBalanceResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	account, err := h.ledger.Account(ctx, h.reader, p.LegalEntityID, ids.ID(req.Id))
	if err != nil {
		return nil, err
	}
	if at, ok := balanceCut(req.Params); ok {
		return h.balanceAsOf(ctx, account, at)
	}
	b, err := h.ledger.Balance(ctx, h.reader, account.LegalEntityID, account.ID)
	if err != nil {
		return nil, err
	}
	pendingDebits, pendingCredits := moneyJSON(b.PendingDebits), moneyJSON(b.PendingCredits)
	return gen.GetLedgerAccountBalance200JSONResponse{
		AccountId:      [16]byte(account.ID),
		Posted:         moneyJSON(b.Posted),
		Natural:        moneyJSON(ledger.NaturalAmount(account.Kind, b.Posted)),
		PendingDebits:  &pendingDebits,
		PendingCredits: &pendingCredits,
		Version:        &b.Version,
	}, nil
}

// balanceCut reads the cutoffs of the bitemporal balance. An empty parameter (?as_known_at=) arrives as the
// zero time and counts as absent: with both absent the materialized balance is requested. The knowledge
// cutoff is kept in UTC and in microseconds, the precision of posted_at.
func balanceCut(p gen.GetLedgerAccountBalanceParams) (ledger.BalanceAsOf, bool) {
	var at ledger.BalanceAsOf
	if p.AsOfValueDate != nil && !p.AsOfValueDate.IsZero() {
		at.ValueDate = &p.AsOfValueDate.Time
	}
	if p.AsKnownAt != nil && !p.AsKnownAt.IsZero() {
		known := p.AsKnownAt.UTC().Truncate(time.Microsecond)
		at.KnownAt = &known
	}
	return at, at.ValueDate != nil || at.KnownAt != nil
}

// balanceAsOf is the bitemporal balance (FR-8): it returns the cutoffs it was computed with and carries no
// pending amounts, which only exist in the present.
func (h *LedgerHandlers) balanceAsOf(ctx context.Context, account ledger.Account, at ledger.BalanceAsOf) (gen.GetLedgerAccountBalanceResponseObject, error) {
	posted, err := h.ledger.PostedAsOf(ctx, h.reader, account.LegalEntityID, account.ID, at)
	if err != nil {
		return nil, err
	}
	b := gen.GetLedgerAccountBalance200JSONResponse{
		AccountId: [16]byte(account.ID),
		Posted:    moneyJSON(posted),
		Natural:   moneyJSON(ledger.NaturalAmount(account.Kind, posted)),
		AsKnownAt: at.KnownAt,
	}
	if at.ValueDate != nil {
		b.AsOfValueDate = &openapi_types.Date{Time: *at.ValueDate}
	}
	return b, nil
}

// PostTransaction posts a balanced transaction of the token's entity once per Idempotency-Key, or, if it reaches the
// threshold of manual postings (ADR-0019), checks it and leaves it as a ledger.manual_posting request
// (202) until another person approves it.
func (h *LedgerHandlers) PostTransaction(ctx context.Context, req gen.PostTransactionRequestObject) (gen.PostTransactionResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	in, err := manualInput(*req.Body, p)
	if err != nil {
		return nil, err
	}
	resp, err := h.commands.idempotent(ctx, p, postTransaction, req.Params.IdempotencyKey, req.Body, func(tx postgres.Tx) (any, error) {
		if !h.manualThresholds.reached(in.Entries) {
			posted, err := postManual(ctx, h.ledger, h.deposits, tx, in)
			if err != nil {
				return nil, err
			}
			return toTransactionJSON(posted), nil
		}
		if err := checkManual(ctx, h.ledger, h.deposits, tx, in); err != nil {
			return nil, err
		}
		requested, err := requestApproval(ctx, h.approvals, tx, manualPostingKind, *req.Body, p)
		if err != nil {
			return nil, err
		}
		return withStatus{status: http.StatusAccepted, body: requested}, nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// GetTransaction reads a transaction of the token's entity.
func (h *LedgerHandlers) GetTransaction(ctx context.Context, req gen.GetTransactionRequestObject) (gen.GetTransactionResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := h.ledger.Transaction(ctx, h.reader, p.LegalEntityID, ids.ID(req.Id))
	if err != nil {
		return nil, err
	}
	return gen.GetTransaction200JSONResponse(toTransactionJSON(tx)), nil
}

// reversalRequest is the payload of a reversal request: what the checker approves and what gets posted.
type reversalRequest struct {
	TransactionID ids.ID              `json:"transaction_id"`
	Reason        string              `json:"reason"`
	BusinessDate  *openapi_types.Date `json:"business_date,omitempty"`
}

// RequestReversal requests the reversal of a transaction of the token's entity: it stays as a
// ledger.reversal request until another person approves it (FR-39), once per Idempotency-Key. That of a
// reversal, or of a transaction already reversed, is rejected when requested, not left to fail once approved
// (ledger.Reverse refuses both too): to apply the original again, it is posted anew.
func (h *LedgerHandlers) RequestReversal(ctx context.Context, req gen.RequestReversalRequestObject) (gen.RequestReversalResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	id := ids.ID(req.Id)
	op := resourceOperation("transactions", id, "reversal", http.StatusAccepted)
	noteTargets(ctx, op.targets) // before validating the body: its rejection also names the transaction
	if strings.TrimSpace(req.Body.Reason) == "" {
		return nil, fmt.Errorf("%w: missing reversal reason", ledger.ErrInvalidPosting)
	}
	payload := reversalRequest{TransactionID: id, Reason: req.Body.Reason, BusinessDate: req.Body.BusinessDate}
	resp, err := h.commands.idempotent(ctx, p, op, req.Params.IdempotencyKey, req.Body, func(tx postgres.Tx) (any, error) {
		original, err := h.ledger.Transaction(ctx, tx, p.LegalEntityID, id)
		if err != nil {
			return nil, err
		}
		if original.ReversesID != (ids.ID{}) {
			return nil, fmt.Errorf("%w: %s reverses %s", ledger.ErrReversalOfReversal, id, original.ReversesID)
		}
		reversal, reversed, err := h.ledger.ReversalOf(ctx, tx, p.LegalEntityID, id)
		switch {
		case err != nil:
			return nil, err
		case reversed:
			return nil, fmt.Errorf("%w: %s is reversed by %s", ledger.ErrAlreadyReversed, id, reversal.ID)
		}
		return requestApproval(ctx, h.approvals, tx, reversalKind, payload, p)
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// reverseOnApproval is the Executor of reversalKind: it posts the payload's reversal in the entity of the
// approver, who becomes its actor, after locking the deposit accounts it touches. A ledger rejection (a 4xx from
// its problems table: already reversed, period closed, insufficient funds) or a deposit account that is no longer
// active leaves the request failed; any other error goes up.
func reverseOnApproval(l *ledger.Service, d *deposits.Service) approvals.Executor {
	return onApproval("reversal", func(ctx context.Context, tx postgres.Tx, r reversalRequest, checker access.Principal) (gen.Transaction, error) {
		original, err := l.Transaction(ctx, tx, checker.LegalEntityID, r.TransactionID)
		if err != nil {
			return gen.Transaction{}, err
		}
		accounts := make([]ids.ID, len(original.Entries))
		for i, e := range original.Entries {
			accounts[i] = e.AccountID
		}
		if err := d.LockActiveHeldIn(ctx, tx, checker.LegalEntityID, accounts); err != nil {
			return gen.Transaction{}, err
		}
		in := ledger.ReverseInput{LegalEntityID: checker.LegalEntityID, TransactionID: r.TransactionID, Reason: r.Reason, Actor: checker.Subject, Channel: apiChannel}
		if r.BusinessDate != nil {
			in.BusinessDate = r.BusinessDate.Time
		}
		reversal, err := l.Reverse(ctx, tx, in)
		if err != nil {
			return gen.Transaction{}, err
		}
		return toTransactionJSON(reversal), nil
	})
}

func toAccountJSON(a ledger.Account) gen.LedgerAccount {
	return gen.LedgerAccount{
		Id: [16]byte(a.ID), Code: a.Code, Kind: gen.AccountKind(a.Kind), Currency: a.Currency.Code(),
		BalanceMode: gen.BalanceMode(a.Mode), LowerLimit: a.LowerLimit,
	}
}

func toTransactionJSON(tx ledger.Transaction) gen.Transaction {
	entries := make([]gen.Entry, len(tx.Entries))
	for i, e := range tx.Entries {
		entries[i] = gen.Entry{AccountId: [16]byte(e.AccountID), Amount: moneyJSON(e.Amount)}
	}
	return gen.Transaction{
		Id: [16]byte(tx.ID), Code: tx.Code, ValueDate: dateOf(tx.ValueDate), BusinessDate: dateOf(tx.BusinessDate),
		PostedAt: tx.PostedAt, Source: tx.Source, ExternalRef: tx.ExternalRef, Entries: entries,
	}
}

func dateOf(t time.Time) openapi_types.Date { return openapi_types.Date{Time: t} }

// moduleSources are the sources the modules post their own movements with: their external references are theirs
// (a movement's, an order's), so a manual posting that took one would take the place of the module's.
var moduleSources = [...]string{deposits.Source, payments.Source}

// moduleSource reports whether source is a module's, or one of its sub-sources ("deposits.capitalization").
func moduleSource(source string) bool {
	for _, m := range moduleSources {
		if source == m || strings.HasPrefix(source, m+".") {
			return true
		}
	}
	return false
}

// requireBusinessDate rejects the zero date (0001-01-01): the ledger would take it as absent and use the
// entity's, and through the API the caller sends the business date.
func requireBusinessDate(d openapi_types.Date) error {
	if d.IsZero() {
		return fmt.Errorf("%w: missing business date", ledger.ErrInvalidPosting)
	}
	return nil
}

// amountOf converts a contract amount; an unknown currency is ErrUnknownCurrency.
func amountOf(m gen.Money) (money.Amount, error) {
	cur, err := money.CurrencyOf(m.Currency)
	if err != nil {
		return money.Amount{}, fmt.Errorf("currency: %w", err)
	}
	return money.New(m.Amount, cur), nil
}

func moneyJSON(a money.Amount) gen.Money {
	return gen.Money{Amount: a.Units(), Currency: a.Currency().Code()}
}

// PlaceHold holds funds of an account of the token's entity, once per Idempotency-Key.
func (h *LedgerHandlers) PlaceHold(ctx context.Context, req gen.PlaceHoldRequestObject) (gen.PlaceHoldResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	amount, err := amountOf(req.Body.Amount)
	if err != nil {
		return nil, fmt.Errorf("hold: %w", err)
	}
	in := ledger.PlaceHoldInput{
		LegalEntityID: p.LegalEntityID, AccountID: ids.ID(req.Body.AccountId), CounterAccountID: ids.ID(req.Body.CounterAccountId),
		Amount: amount, ExpiresAt: req.Body.ExpiresAt,
	}
	resp, err := h.commands.idempotent(ctx, p, placeHold, req.Params.IdempotencyKey, req.Body, func(tx postgres.Tx) (any, error) {
		if err := h.deposits.LockActiveHeldIn(ctx, tx, p.LegalEntityID, []ids.ID{in.AccountID, in.CounterAccountID}); err != nil {
			return nil, err
		}
		placed, err := h.ledger.PlaceHold(ctx, tx, in)
		if err != nil {
			return nil, err
		}
		return toHoldJSON(placed), nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// CaptureHold posts part or all of a hold of the token's entity, once per Idempotency-Key.
func (h *LedgerHandlers) CaptureHold(ctx context.Context, req gen.CaptureHoldRequestObject) (gen.CaptureHoldResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	op := resourceOperation("holds", ids.ID(req.Id), "capture", http.StatusOK)
	noteTargets(ctx, op.targets) // before validating the body: its rejection also names the hold
	if err := requireBusinessDate(req.Body.BusinessDate); err != nil {
		return nil, err
	}
	amount, err := amountOf(req.Body.Amount)
	if err != nil {
		return nil, fmt.Errorf("capture: %w", err)
	}
	in := ledger.CaptureInput{
		LegalEntityID: p.LegalEntityID, HoldID: ids.ID(req.Id), Amount: amount,
		BusinessDate: req.Body.BusinessDate.Time, Actor: p.Subject, Channel: apiChannel,
	}
	resp, err := h.commands.idempotent(ctx, p, op, req.Params.IdempotencyKey, req.Body, func(tx postgres.Tx) (any, error) {
		hold, err := h.ledger.Hold(ctx, tx, p.LegalEntityID, in.HoldID)
		if err != nil {
			return nil, err
		}
		if err := h.deposits.LockActiveHeldIn(ctx, tx, p.LegalEntityID, []ids.ID{hold.AccountID(), hold.CounterAccountID()}); err != nil {
			return nil, err
		}
		captured, posted, err := h.ledger.CaptureHold(ctx, tx, in)
		if err != nil {
			return nil, err
		}
		return gen.HoldCapture{Hold: toHoldJSON(captured), Transaction: toTransactionJSON(posted)}, nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// ReleaseHold releases a hold of the token's entity, once per Idempotency-Key.
func (h *LedgerHandlers) ReleaseHold(ctx context.Context, req gen.ReleaseHoldRequestObject) (gen.ReleaseHoldResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	in := ledger.ReleaseInput{LegalEntityID: p.LegalEntityID, HoldID: ids.ID(req.Id)}
	op := resourceOperation("holds", ids.ID(req.Id), "release", http.StatusOK)
	resp, err := h.commands.idempotent(ctx, p, op, req.Params.IdempotencyKey, nil, func(tx postgres.Tx) (any, error) {
		released, err := h.ledger.ReleaseHold(ctx, tx, in)
		if err != nil {
			return nil, err
		}
		return toHoldJSON(released), nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func toHoldJSON(hold ledger.Hold) gen.Hold {
	return gen.Hold{
		Id: [16]byte(hold.ID()), AccountId: [16]byte(hold.AccountID()), CounterAccountId: [16]byte(hold.CounterAccountID()),
		Amount: moneyJSON(hold.Amount()), Captured: moneyJSON(hold.Captured()), Status: gen.HoldStatus(hold.Status()),
		ExpiresAt: hold.ExpiresAt().UTC(),
	}
}
