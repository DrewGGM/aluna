package httpapi

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/DrewGGM/aluna/internal/access"
	"github.com/DrewGGM/aluna/internal/approvals"
	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/compliance"
	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// transferChannel is the channel recorded with the transfers that come in through the API, and
// compliance.ChannelCash the one of cash deposits and withdrawals, wherever they are called from; who called is
// the actor.
const transferChannel = "api"

var openDepositAccount = operation{method: http.MethodPost, path: "/v1/deposit-accounts", success: http.StatusCreated}

// errInvalidAccountStatus is a status change to other than active, frozen or closed: the generated code does not
// reject it, and the domain would take dormant, which only the dormancy job sets (FR-27).
var errInvalidAccountStatus = errors.New("a deposit account's status changes to active, frozen or closed")

// Maker-checker request kind of freezing and unfreezing a deposit account (a blocking order and its lifting,
// architecture §5) and the permission to request and to approve it.
const (
	statusChangeKind       = "deposits.account_status_change"
	statusChangePermission = "deposits.accounts.change_status"
)

// DepositHandlers implements the contract's deposit account operations (api/openapi.yaml).
type DepositHandlers struct {
	deposits  *deposits.Service
	ledger    *ledger.Service
	approvals *approvals.Service
	commands  commands
	reader    postgres.Querier
}

// NewDepositHandlers creates the deposit handlers and registers in a the freeze and unfreeze request kind, which
// goes through maker-checker; l reads the accounts' history. keys stores the responses of the commands with
// Idempotency-Key; reader serves the reads outside a transaction.
func NewDepositHandlers(d *deposits.Service, l *ledger.Service, a *approvals.Service, uow *postgres.UnitOfWork, keys *idempotency.Store, reader postgres.Querier) *DepositHandlers {
	a.RegisterKind(statusChangeKind, statusChangePermission, changeStatusOnApproval(d))
	return &DepositHandlers{deposits: d, ledger: l, approvals: a, commands: commands{uow: uow, keys: keys}, reader: reader}
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

// ChangeDepositAccountStatus moves an account of the token's entity to the requested status, once per
// Idempotency-Key: a freeze or an unfreeze waits as a deposits.account_status_change request until someone else
// approves it (FR-39) and answers 202 with it; any other change is made at once, with the token's subject as who
// made it, and answers the account as it stands afterwards.
func (h *DepositHandlers) ChangeDepositAccountStatus(ctx context.Context, req gen.ChangeDepositAccountStatusRequestObject) (gen.ChangeDepositAccountStatusResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	id := ids.ID(req.Id)
	op := resourceOperation("deposit-accounts", id, "status-changes", http.StatusOK)
	noteTargets(ctx, op.targets) // before validating the body: its rejection also names the account
	if !req.Body.Status.Valid() {
		return nil, fmt.Errorf("%w, not %q", errInvalidAccountStatus, req.Body.Status)
	}
	change := statusChange{AccountID: id, Status: deposits.Status(req.Body.Status)}
	resp, err := h.commands.idempotent(ctx, p, op, req.Params.IdempotencyKey, req.Body, func(tx postgres.Tx) (any, error) {
		return h.changeStatus(ctx, tx, p, change)
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// statusChange is an account, the status it goes to and how many status changes the account had when it was
// requested: the payload of a freeze or unfreeze request, kept as evidence, and what approving it returns
// (approvals.read does not grant reading accounts).
type statusChange struct {
	AccountID ids.ID          `json:"account_id"`
	Status    deposits.Status `json:"status"`
	Changes   int             `json:"status_changes"`
}

// changeStatus makes change for p within tx, deciding under the account's lock: a freeze or an unfreeze becomes a
// request (202), so a concurrent change cannot turn a direct change into an unfreeze nobody approved; any other change
// is made at once and answers the account.
func (h *DepositHandlers) changeStatus(ctx context.Context, tx postgres.Tx, p access.Principal, change statusChange) (any, error) {
	a, err := h.deposits.LockAccount(ctx, tx, p.LegalEntityID, change.AccountID)
	if err != nil {
		return nil, err
	}
	if needsApproval(a.Status, change.Status) {
		if change.Changes, err = h.deposits.StatusChanges(ctx, tx, p.LegalEntityID, a.ID); err != nil {
			return nil, err
		}
		requested, err := h.approvals.Request(ctx, tx, statusChangeKind, change, p)
		if err != nil {
			return nil, err
		}
		return withStatus{status: http.StatusAccepted, body: toApprovalJSON(requested)}, nil
	}
	if err := h.deposits.ChangeStatus(ctx, tx, p.LegalEntityID, a.ID, change.Status, p.Subject); err != nil {
		return nil, err
	}
	if a, err = h.deposits.Account(ctx, tx, p.LegalEntityID, a.ID); err != nil {
		return nil, err
	}
	return toDepositAccountJSON(a), nil
}

// needsApproval reports whether moving an account from from to to is a freeze or an unfreeze, which the diagram puts
// under maker-checker (architecture §5). Any other change is made at once, or rejected by ChangeStatus.
func needsApproval(from, to deposits.Status) bool {
	return (from == deposits.Active && to == deposits.Frozen) || (from == deposits.Frozen && to == deposits.Active)
}

// changeStatusOnApproval is the Executor of statusChangeKind: it makes the change in the approver's entity, with the
// approver as who made it, if the account has not changed status since the request. One that did (closed, or frozen
// or unfrozen by another request, even back to the status it had) is ErrInvalidTransition, and the request is left
// failed: an old unfreeze does not lift a later freeze.
func changeStatusOnApproval(d *deposits.Service) approvals.Executor {
	return onApproval("deposit account status change", func(ctx context.Context, tx postgres.Tx, r statusChange, checker access.Principal) (statusChange, error) {
		a, err := d.LockAccount(ctx, tx, checker.LegalEntityID, r.AccountID)
		if err != nil {
			return statusChange{}, err
		}
		changes, err := d.StatusChanges(ctx, tx, checker.LegalEntityID, a.ID)
		if err != nil {
			return statusChange{}, err
		}
		if changes != r.Changes {
			return statusChange{}, fmt.Errorf("%w: account %s changed status after the change to %s was requested",
				deposits.ErrInvalidTransition, a.ID, r.Status)
		}
		if err := d.ChangeStatus(ctx, tx, checker.LegalEntityID, a.ID, r.Status, checker.Subject); err != nil {
			return statusChange{}, err
		}
		return r, nil
	})
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
	cursor, limit := pageOf(req.Params.Cursor, req.Params.Limit)
	page, err := h.ledger.History(ctx, h.reader, p.LegalEntityID, a.LedgerAccountID, cursor, limit)
	if err != nil {
		return nil, err
	}
	return gen.ListDepositAccountTransactions200JSONResponse(toHistoryJSON(page)), nil
}

// GetDepositAccountStatement reads an account's statement of a month, of the token's entity, at no cost (FR-45).
func (h *DepositHandlers) GetDepositAccountStatement(ctx context.Context, req gen.GetDepositAccountStatementRequestObject) (gen.GetDepositAccountStatementResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if err := calendar.ValidatePeriod(req.Period); err != nil {
		return nil, err
	}
	st, err := h.deposits.Statement(ctx, h.reader, p.LegalEntityID, ids.ID(req.Id), req.Period)
	if err != nil {
		return nil, err
	}
	return gen.GetDepositAccountStatement200JSONResponse{
		Period: req.Period, ContentHash: st.ContentHash, GeneratedAt: st.GeneratedAt, Document: st.Document,
	}, nil
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
		Actor: p.Subject, Channel: compliance.ChannelCash,
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
		ExternalRef: movementRef(p, op, key), Actor: p.Subject, Channel: transferChannel,
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

func dateJSON(d calendar.Date) openapi_types.Date { return dateOf(d.Time()) }
