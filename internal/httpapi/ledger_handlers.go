package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/DrewGGM/aluna/internal/access"
	"github.com/DrewGGM/aluna/internal/approvals"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// Origin of the manual postings that come in through POST /v1/transactions. They are stored forever in
// ledger.transactions: rule_version "manual.v1" says the entries came built by the caller and not from a
// versioned posting rule. Who called is kept in the audit record.
const (
	manualActor       = "api"
	manualChannel     = "api"
	manualRuleVersion = "manual.v1"
)

// Maker-checker request kind of a reversal (FR-3, FR-39) and the permission required to approve it.
const (
	reversalKind       = "ledger.reversal"
	reversalPermission = "ledger.transactions.reverse"
)

// LedgerHandlers implements the contract's ledger operations (api/openapi.yaml).
type LedgerHandlers struct {
	ledger    *ledger.Service
	approvals *approvals.Service
	commands  commands
	reader    postgres.Querier
}

// NewLedgerHandlers creates the ledger handlers and registers in a the reversal request kind, which goes
// through maker-checker. keys stores the responses of the commands with Idempotency-Key; reader serves the
// reads outside a transaction. The legal entity of each request is that of its token (FR-41).
func NewLedgerHandlers(l *ledger.Service, a *approvals.Service, uow *postgres.UnitOfWork, keys *idempotency.Store, reader postgres.Querier) *LedgerHandlers {
	a.RegisterKind(reversalKind, reversalPermission, reverseOnApproval(l))
	return &LedgerHandlers{ledger: l, approvals: a, commands: commands{uow: uow, keys: keys}, reader: reader}
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
	b, err := h.ledger.Balance(ctx, h.reader, account.ID)
	if err != nil {
		return nil, err
	}
	cur := account.Currency.Code()
	return gen.GetLedgerAccountBalance200JSONResponse{
		AccountId:      [16]byte(account.ID),
		Posted:         gen.Money{Amount: b.Posted, Currency: cur},
		Natural:        gen.Money{Amount: ledger.NaturalBalance(account.Kind, b.Posted), Currency: cur},
		PendingDebits:  &gen.Money{Amount: b.PendingDebits, Currency: cur},
		PendingCredits: &gen.Money{Amount: b.PendingCredits, Currency: cur},
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
	posted, err := h.ledger.PostedAsOf(ctx, h.reader, account.ID, at)
	if err != nil {
		return nil, err
	}
	cur := account.Currency.Code()
	b := gen.GetLedgerAccountBalance200JSONResponse{
		AccountId: [16]byte(account.ID),
		Posted:    gen.Money{Amount: posted, Currency: cur},
		Natural:   gen.Money{Amount: ledger.NaturalBalance(account.Kind, posted), Currency: cur},
		AsKnownAt: at.KnownAt,
	}
	if at.ValueDate != nil {
		b.AsOfValueDate = &openapi_types.Date{Time: *at.ValueDate}
	}
	return b, nil
}

// PostTransaction posts a manual transaction of the token's entity, once per Idempotency-Key.
func (h *LedgerHandlers) PostTransaction(ctx context.Context, req gen.PostTransactionRequestObject) (gen.PostTransactionResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if err := requireBusinessDate(req.Body.BusinessDate); err != nil {
		return nil, err
	}
	entries := make([]ledger.EntryInput, len(req.Body.Entries))
	for i, e := range req.Body.Entries {
		amount, err := amountOf(e.Amount)
		if err != nil {
			return nil, fmt.Errorf("entry %d: %w", i, err)
		}
		entries[i] = ledger.EntryInput{AccountID: ids.ID(e.AccountId), Amount: amount}
	}
	in := ledger.PostInput{
		LegalEntityID: p.LegalEntityID, Code: req.Body.Code, ValueDate: req.Body.ValueDate.Time,
		BusinessDate: req.Body.BusinessDate.Time, Source: req.Body.Source, ExternalRef: req.Body.ExternalRef,
		Actor: manualActor, Channel: manualChannel, RuleVersion: manualRuleVersion, Entries: entries,
	}
	resp, err := h.commands.idempotent(ctx, p, postTransaction, req.Params.IdempotencyKey, req.Body, func(tx postgres.Tx) (any, error) {
		posted, err := h.ledger.Post(ctx, tx, in)
		if err != nil {
			return nil, err
		}
		return toTransactionJSON(posted), nil
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

// errReversalOfReversal means requesting the reversal of a transaction that is already a reversal:
// approving it would apply the original again without the checker seeing it in the payload.
var errReversalOfReversal = errors.New("the transaction is a reversal: it cannot be reversed")

// RequestReversal requests the reversal of a transaction of the token's entity: it stays as a
// ledger.reversal request until another person approves it (FR-39), once per Idempotency-Key. That of a
// reversal is rejected: to apply the original again, it is posted anew.
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
			return nil, fmt.Errorf("%w: %s reverses %s", errReversalOfReversal, id, original.ReversesID)
		}
		requested, err := h.approvals.Request(ctx, tx, reversalKind, payload, p)
		if err != nil {
			return nil, err
		}
		return toApprovalJSON(requested), nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// reverseOnApproval is the Executor of reversalKind: it posts the payload's reversal in the entity of the
// approver, who becomes its actor. A ledger rejection (a 4xx from its problems table: already reversed,
// period closed, insufficient funds) is wrapped in approvals.ErrCommandRejected and leaves the request
// failed; any other error goes up.
func reverseOnApproval(l *ledger.Service) approvals.Executor {
	return func(ctx context.Context, tx postgres.Tx, payload json.RawMessage, checker access.Principal) (json.RawMessage, error) {
		var r reversalRequest
		if err := json.Unmarshal(payload, &r); err != nil {
			return nil, fmt.Errorf("reversal payload: %w", err)
		}
		in := ledger.ReverseInput{LegalEntityID: checker.LegalEntityID, TransactionID: r.TransactionID, Reason: r.Reason, Actor: checker.Subject}
		if r.BusinessDate != nil {
			in.BusinessDate = r.BusinessDate.Time
		}
		reversal, err := l.Reverse(ctx, tx, in)
		if err != nil {
			if p, known := problemFor(err); known && p.Status < http.StatusInternalServerError {
				return nil, fmt.Errorf("%w: %w", approvals.ErrCommandRejected, err)
			}
			return nil, err
		}
		result, err := json.Marshal(toTransactionJSON(reversal))
		if err != nil {
			return nil, fmt.Errorf("reversal result: %w", err)
		}
		return result, nil
	}
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
		BusinessDate: req.Body.BusinessDate.Time, Actor: manualActor, Channel: manualChannel,
	}
	resp, err := h.commands.idempotent(ctx, p, op, req.Params.IdempotencyKey, req.Body, func(tx postgres.Tx) (any, error) {
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
