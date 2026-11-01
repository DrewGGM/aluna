package httpapi

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/DrewGGM/aluna/internal/access"
	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// movementChannel is the channel recorded with the movements that come in through the API; who called is
// the actor.
const movementChannel = "api"

// defaultPageSize is the page of history of a request without limit.
const defaultPageSize = 50

var openDepositAccount = operation{method: http.MethodPost, path: "/v1/deposit-accounts", success: http.StatusCreated}

// DepositHandlers implements the contract's deposit account operations (api/openapi.yaml).
type DepositHandlers struct {
	deposits *deposits.Service
	ledger   *ledger.Service
	commands commands
	reader   postgres.Querier
}

// NewDepositHandlers creates the deposit handlers; l reads the accounts' history. keys stores the responses
// of the commands with Idempotency-Key; reader serves the reads outside a transaction.
func NewDepositHandlers(d *deposits.Service, l *ledger.Service, uow *postgres.UnitOfWork, keys *idempotency.Store, reader postgres.Querier) *DepositHandlers {
	return &DepositHandlers{deposits: d, ledger: l, commands: commands{uow: uow, keys: keys}, reader: reader}
}

// OpenDepositAccount opens an account of the token's entity, once per Idempotency-Key.
func (h *DepositHandlers) OpenDepositAccount(ctx context.Context, req gen.OpenDepositAccountRequestObject) (gen.OpenDepositAccountResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	in := deposits.OpenInput{LegalEntityID: p.LegalEntityID, PartyID: ids.ID(req.Body.PartyId), ProductID: ids.ID(req.Body.ProductId)}
	resp, err := h.commands.idempotent(ctx, p, openDepositAccount, req.Params.IdempotencyKey, req.Body, func(tx postgres.Tx) (any, error) {
		a, err := h.deposits.Open(ctx, tx, in)
		if err != nil {
			return nil, err
		}
		return toDepositAccountJSON(a), nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// GetDepositAccount reads an account of the token's entity.
func (h *DepositHandlers) GetDepositAccount(ctx context.Context, req gen.GetDepositAccountRequestObject) (gen.GetDepositAccountResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	a, err := h.deposits.Account(ctx, h.reader, p.LegalEntityID, ids.ID(req.Id))
	if err != nil {
		return nil, err
	}
	return gen.GetDepositAccount200JSONResponse(toDepositAccountJSON(a)), nil
}

// ListDepositAccountTransactions reads a page of the history of an account of the token's entity.
func (h *DepositHandlers) ListDepositAccountTransactions(ctx context.Context, req gen.ListDepositAccountTransactionsRequestObject) (gen.ListDepositAccountTransactionsResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	a, err := h.deposits.Account(ctx, h.reader, p.LegalEntityID, ids.ID(req.Id))
	if err != nil {
		return nil, err
	}
	cursor, limit := "", defaultPageSize
	if req.Params.Cursor != nil {
		cursor = *req.Params.Cursor
	}
	if req.Params.Limit != nil {
		limit = *req.Params.Limit
	}
	page, err := h.ledger.History(ctx, h.reader, p.LegalEntityID, a.LedgerAccountID, cursor, limit)
	if err != nil {
		return nil, err
	}
	return gen.ListDepositAccountTransactions200JSONResponse(toHistoryJSON(page)), nil
}

func toHistoryJSON(page ledger.HistoryPage) gen.AccountHistory {
	entries := make([]gen.HistoryEntry, len(page.Entries))
	for i, e := range page.Entries {
		entries[i] = gen.HistoryEntry{
			TransactionId: [16]byte(e.TransactionID), Code: e.Code, Amount: moneyJSON(e.Amount),
			ValueDate: dateOf(e.ValueDate), PostedAt: e.PostedAt,
		}
	}
	h := gen.AccountHistory{Entries: entries}
	if page.Next != "" {
		h.NextCursor = &page.Next
	}
	return h
}

// CreateDeposit posts cash into an account of the token's entity, once per Idempotency-Key.
func (h *DepositHandlers) CreateDeposit(ctx context.Context, req gen.CreateDepositRequestObject) (gen.CreateDepositResponseObject, error) {
	resp, err := h.cash(ctx, ids.ID(req.Id), "deposits", req.Params.IdempotencyKey, cashRequest{req.Body, req.Body.Amount, nil},
		h.deposits.Deposit)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// CreateWithdrawal posts cash out of an account of the token's entity, with its fees and its GMF, once per
// Idempotency-Key.
func (h *DepositHandlers) CreateWithdrawal(ctx context.Context, req gen.CreateWithdrawalRequestObject) (gen.CreateWithdrawalResponseObject, error) {
	resp, err := h.cash(ctx, ids.ID(req.Id), "withdrawals", req.Params.IdempotencyKey,
		cashRequest{req.Body, req.Body.Amount, req.Body.QuoteId}, h.deposits.Withdraw)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// cashRequest is the body of a deposit or a withdrawal, with its amount and, of a withdrawal, its quote.
type cashRequest struct {
	body   any
	amount gen.Money
	quote  *openapi_types.UUID
}

// cash runs move, a deposit or a withdrawal on account id, as the action action on it.
func (h *DepositHandlers) cash(ctx context.Context, id ids.ID, action, key string, r cashRequest,
	move func(context.Context, postgres.Tx, deposits.CashInput) (ledger.Transaction, error),
) (storedResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return storedResponse{}, err
	}
	op := resourceOperation("deposit-accounts", id, action, http.StatusCreated)
	noteTargets(ctx, op.targets) // before validating the body: its rejection also names the account
	amount, err := amountOf(r.amount)
	if err != nil {
		return storedResponse{}, fmt.Errorf("%s: %w", action, err)
	}
	in := deposits.CashInput{
		LegalEntityID: p.LegalEntityID, AccountID: id, QuoteID: quoteIDOf(r.quote), Amount: amount, ExternalRef: movementRef(p, op, key),
		Actor: p.Subject, Channel: movementChannel,
	}
	return h.commands.idempotent(ctx, p, op, key, r.body, func(tx postgres.Tx) (any, error) {
		posted, err := move(ctx, tx, in)
		if err != nil {
			return nil, err
		}
		return toTransactionJSON(posted), nil
	})
}

// CreateTransfer moves money between two accounts of the token's entity, once per Idempotency-Key.
func (h *DepositHandlers) CreateTransfer(ctx context.Context, req gen.CreateTransferRequestObject) (gen.CreateTransferResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	from, to := ids.ID(req.Body.FromAccountId), ids.ID(req.Body.ToAccountId)
	op := operation{method: http.MethodPost, path: "/v1/transfers", success: http.StatusCreated, targets: []string{from.String(), to.String()}}
	noteTargets(ctx, op.targets) // before validating the body: its rejection also names the accounts
	amount, err := amountOf(req.Body.Amount)
	if err != nil {
		return nil, fmt.Errorf("transfer: %w", err)
	}
	key := req.Params.IdempotencyKey
	in := deposits.TransferInput{
		LegalEntityID: p.LegalEntityID, From: from, To: to, QuoteID: quoteIDOf(req.Body.QuoteId), Amount: amount,
		ExternalRef: movementRef(p, op, key), Actor: p.Subject, Channel: movementChannel,
	}
	resp, err := h.commands.idempotent(ctx, p, op, key, req.Body, func(tx postgres.Tx) (any, error) {
		posted, err := h.deposits.Transfer(ctx, tx, in)
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

// quoteIDOf is the quote a movement names, or the zero ID if it names none: it is priced as it is done.
func quoteIDOf(id *openapi_types.UUID) ids.ID {
	if id == nil {
		return ids.ID{}
	}
	return ids.ID(*id)
}

// movementRef is the ledger reference of the movement op does for p with Idempotency-Key key: the same key on
// the same resource always gives the same one, so a retry after the key store forgot the key (7 days) is a
// duplicate in the ledger and not a second movement.
func movementRef(p access.Principal, op operation, key string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(keyScope(p, op)+"\n"+key)))
}

func toDepositAccountJSON(a deposits.Account) gen.DepositAccount {
	return gen.DepositAccount{
		Id: [16]byte(a.ID), PartyId: [16]byte(a.PartyID), ProductVersionId: [16]byte(a.ProductVersionID),
		LedgerAccountId: [16]byte(a.LedgerAccountID), Currency: a.Currency.Code(), Status: gen.DepositAccountStatus(a.Status),
		OpenedOn: dateJSON(a.OpenedOn), LastCustomerActivityOn: dateJSON(a.LastCustomerActivityOn),
	}
}

func dateJSON(d calendar.Date) openapi_types.Date {
	return dateOf(time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC))
}
