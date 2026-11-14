package httpapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/DrewGGM/aluna/internal/access"
	"github.com/DrewGGM/aluna/internal/approvals"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/payments"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// PaymentHandlers implements the contract's payment order operations (api/openapi.yaml).
type PaymentHandlers struct {
	payments  *payments.Service
	approvals *approvals.Service
	commands  commands
	reader    postgres.Querier
}

// The request kinds of resolving a transfer held in suspense, crediting it or returning it to its bank, and who
// decides each (config/roles.yaml).
const (
	suspenseCreditKind       = "payments.suspense_credit"
	suspenseCreditPermission = "payments.suspense.credit"
	suspenseReturnKind       = "payments.return_to_sender"
	suspenseReturnPermission = "payments.suspense.return"
)

// NewPaymentHandlers creates the payment handlers and registers in a the request kinds of crediting a transfer held
// in suspense and of returning it to its bank, which go through maker-checker; keys stores the responses of the
// commands with Idempotency-Key, and reader serves the reads outside a transaction.
func NewPaymentHandlers(p *payments.Service, a *approvals.Service, uow *postgres.UnitOfWork, keys *idempotency.Store, reader postgres.Querier) *PaymentHandlers {
	a.RegisterKind(suspenseCreditKind, suspenseCreditPermission, creditOnApproval(p))
	a.RegisterKind(suspenseReturnKind, suspenseReturnPermission, returnOnApproval(p))
	return &PaymentHandlers{payments: p, approvals: a, commands: commands{uow: uow, keys: keys}, reader: reader}
}

// suspenseCredit is the payload of a request to credit a transfer held in suspense, kept as evidence, and what
// approving it returns: which transfer, the account it is credited to, and what the checker approves (its amount,
// its end-to-end id and why it went to suspense), since a transfer received is not read through the API.
type suspenseCredit struct {
	OrderID    ids.ID    `json:"order_id"`
	AccountID  ids.ID    `json:"account_id"`
	Amount     gen.Money `json:"amount"`
	EndToEndID string    `json:"end_to_end_id"`
	Reason     string    `json:"reason"`
}

// CreditSuspendedTransfer requests the credit of a transfer of the token's entity held in suspense to the account
// someone found for it: a payments.suspense_credit request until another person approves it (FR-32, FR-39), once
// per Idempotency-Key. An order that is not a transfer in suspense is rejected when requested.
func (h *PaymentHandlers) CreditSuspendedTransfer(ctx context.Context, req gen.CreditSuspendedTransferRequestObject) (gen.CreditSuspendedTransferResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	id := ids.ID(req.Id)
	op := resourceOperation("payment-orders", id, "credit", http.StatusAccepted)
	resp, err := h.commands.idempotent(ctx, p, op, req.Params.IdempotencyKey, req.Body, func(tx postgres.Tx) (any, error) {
		o, err := h.payments.Suspended(ctx, tx, p.LegalEntityID, id)
		if err != nil {
			return nil, err
		}
		payload := suspenseCredit{
			OrderID: id, AccountID: ids.ID(req.Body.AccountId), Amount: moneyJSON(o.Amount), EndToEndID: o.EndToEndID, Reason: o.Reason,
		}
		requested, err := h.approvals.Request(ctx, tx, suspenseCreditKind, payload, p)
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

// suspenseReturn is the payload of a request to return a transfer held in suspense to its bank, kept as evidence,
// and what approving it returns: which transfer, why it goes back, and what the checker approves (its amount, its
// end-to-end id and, as for a credit, why it went to suspense).
type suspenseReturn struct {
	OrderID      ids.ID    `json:"order_id"`
	ReturnReason string    `json:"return_reason"`
	Amount       gen.Money `json:"amount"`
	EndToEndID   string    `json:"end_to_end_id"`
	Reason       string    `json:"reason"`
}

// ReturnSuspendedTransfer requests the return of a transfer of the token's entity held in suspense to the bank that
// sent it: a payments.return_to_sender request until another person approves it (FR-32, FR-39), once per
// Idempotency-Key. An order that is not a transfer in suspense, or a reason a rail cannot carry, is rejected when
// requested.
func (h *PaymentHandlers) ReturnSuspendedTransfer(ctx context.Context, req gen.ReturnSuspendedTransferRequestObject) (gen.ReturnSuspendedTransferResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	id := ids.ID(req.Id)
	op := resourceOperation("payment-orders", id, "return-to-sender", http.StatusAccepted)
	noteTargets(ctx, op.targets) // before validating the body: its rejection also names the order
	if err := payments.CheckReturnReason(req.Body.Reason); err != nil {
		return nil, err
	}
	resp, err := h.commands.idempotent(ctx, p, op, req.Params.IdempotencyKey, req.Body, func(tx postgres.Tx) (any, error) {
		o, err := h.payments.Suspended(ctx, tx, p.LegalEntityID, id)
		if err != nil {
			return nil, err
		}
		payload := suspenseReturn{
			OrderID: id, ReturnReason: req.Body.Reason, Amount: moneyJSON(o.Amount), EndToEndID: o.EndToEndID, Reason: o.Reason,
		}
		requested, err := h.approvals.Request(ctx, tx, suspenseReturnKind, payload, p)
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

// returnOnApproval is the Executor of suspenseReturnKind: it returns the transfer to its bank in the approver's
// entity, with the approver as who made it. One no longer in suspense, or a reason the rail cannot carry, leaves the
// request failed.
func returnOnApproval(s *payments.Service) approvals.Executor {
	return onApproval("suspense return", func(ctx context.Context, tx postgres.Tx, r suspenseReturn, checker access.Principal) (suspenseReturn, error) {
		if _, err := s.ReturnSuspended(ctx, tx, checker.LegalEntityID, r.OrderID, r.ReturnReason, checker.Subject); err != nil {
			return suspenseReturn{}, err
		}
		return r, nil
	})
}

// creditOnApproval is the Executor of suspenseCreditKind: it credits the transfer in the approver's entity, with the
// approver as who made it. One no longer in suspense, or an account that cannot take it (not active, another
// currency, over a limit), leaves the request failed.
func creditOnApproval(s *payments.Service) approvals.Executor {
	return onApproval("suspense credit", func(ctx context.Context, tx postgres.Tx, r suspenseCredit, checker access.Principal) (suspenseCredit, error) {
		if _, err := s.CreditSuspended(ctx, tx, checker.LegalEntityID, r.OrderID, r.AccountID, checker.Subject); err != nil {
			return suspenseCredit{}, err
		}
		return r, nil
	})
}

// CreatePaymentOrder initiates an outgoing payment from an account of the token's entity, once per
// Idempotency-Key: a retry after the key store forgot the key carries the same reference, so it does not pay
// again (409 duplicate_payment).
func (h *PaymentHandlers) CreatePaymentOrder(ctx context.Context, req gen.CreatePaymentOrderRequestObject) (gen.CreatePaymentOrderResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	account := ids.ID(req.Body.AccountId)
	op := operation{method: http.MethodPost, path: "/v1/payment-orders", success: http.StatusAccepted, targets: []string{account.String()}}
	noteTargets(ctx, op.targets) // before validating the body: its rejection also names the account
	amount, err := amountOf(req.Body.Amount)
	if err != nil {
		return nil, fmt.Errorf("payment order: %w", err)
	}
	key := req.Params.IdempotencyKey
	in := payments.InitiateInput{
		LegalEntityID: p.LegalEntityID, AccountID: account, QuoteID: quoteIDOf(req.Body.QuoteId), Amount: amount,
		Rail: string(req.Body.Rail), ExternalRef: movementRef(p, op, key),
		Creditor: payments.AccountRef{Number: deref(req.Body.Creditor.AccountNumber), Key: deref(req.Body.Creditor.Key)},
	}
	resp, err := h.commands.idempotent(ctx, p, op, key, req.Body, func(tx postgres.Tx) (any, error) {
		o, err := h.payments.Initiate(ctx, tx, in)
		if err != nil {
			return nil, err
		}
		return toPaymentOrderJSON(o), nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// GetPaymentOrder reads an outgoing payment order of the token's entity.
func (h *PaymentHandlers) GetPaymentOrder(ctx context.Context, req gen.GetPaymentOrderRequestObject) (gen.GetPaymentOrderResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	o, err := h.payments.Order(ctx, h.reader, p.LegalEntityID, ids.ID(req.Id))
	if err != nil {
		return nil, err
	}
	return gen.GetPaymentOrder200JSONResponse(toPaymentOrderJSON(o)), nil
}

func toPaymentOrderJSON(o payments.Order) gen.PaymentOrder {
	j := gen.PaymentOrder{
		Id: openapi_types.UUID(o.ID), AccountId: openapi_types.UUID(o.AccountID), Rail: o.Rail, Amount: moneyJSON(o.Amount),
		Status: gen.PaymentOrderStatus(o.Status), EndToEndId: o.EndToEndID, InstrId: o.InstrID, TxId: o.TxID,
	}
	if o.Status == payments.StatusHeld {
		held := moneyJSON(o.Held)
		j.Held = &held
	}
	if o.RailRef != "" {
		j.RailRef = &o.RailRef
	}
	if o.Reason != "" {
		j.Reason = &o.Reason
	}
	return j
}

// RailRoutes registers in mux the rails' notifications (TH-7): the outcomes of the simulated Bre-B rail and the
// transfers other banks send through it, which it signs instead of carrying a token, so they go on a server of
// their own, without the channels' tokens and permissions. The signature is checked before the database is
// touched; log records the errors that are not the rail's.
func RailRoutes(h *PaymentHandlers, log *slog.Logger) func(*http.ServeMux) {
	answer := responseError(log)
	return func(mux *http.ServeMux) {
		mux.HandleFunc("POST /v1/rails/"+payments.RailSimBreB+"/notifications", signed(answer, h.takeNotification))
		mux.HandleFunc("POST /v1/rails/"+payments.RailSimBreB+"/transfers", signed(answer, h.receiveTransfer))
	}
}

// signed is the handler of a notification of the rail that take applies: 204 if it took it (or had taken it),
// its problem otherwise.
func signed(answer func(http.ResponseWriter, *http.Request, error), take func(context.Context, payments.Notification) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			requestError(w, r, err) // a body over the limit is 413
			return
		}
		if err := take(r.Context(), payments.Notification{Header: r.Header, Body: body}); err != nil {
			answer(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// receiveTransfer reads the transfer that n, a notification of the rail, carries and takes it in its own
// transaction.
func (h *PaymentHandlers) receiveTransfer(ctx context.Context, n payments.Notification) error {
	in, err := h.payments.ReadTransfer(n)
	if err != nil {
		return err
	}
	return h.commands.uow.Do(ctx, func(tx postgres.Tx) error { return h.payments.ReceiveTransfer(ctx, tx, in) })
}

// takeNotification reads the outcome that n, a notification of the rail, carries and applies it in its own
// transaction.
func (h *PaymentHandlers) takeNotification(ctx context.Context, n payments.Notification) error {
	out, err := h.payments.ReadNotification(n)
	if err != nil {
		return err
	}
	return h.commands.uow.Do(ctx, func(tx postgres.Tx) error { return h.payments.TakeOutcome(ctx, tx, out) })
}
