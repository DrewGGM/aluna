package httpapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/payments"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// PaymentHandlers implements the contract's payment order operations (api/openapi.yaml).
type PaymentHandlers struct {
	payments *payments.Service
	commands commands
}

// NewPaymentHandlers creates the payment handlers; keys stores the responses of the commands with
// Idempotency-Key.
func NewPaymentHandlers(p *payments.Service, uow *postgres.UnitOfWork, keys *idempotency.Store) *PaymentHandlers {
	return &PaymentHandlers{payments: p, commands: commands{uow: uow, keys: keys}}
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

func toPaymentOrderJSON(o payments.Order) gen.PaymentOrder {
	j := gen.PaymentOrder{
		Id: openapi_types.UUID(o.ID), AccountId: openapi_types.UUID(o.AccountID), Rail: o.Rail, Amount: moneyJSON(o.Amount),
		Status: gen.PaymentOrderStatus(o.Status), EndToEndId: o.EndToEndID, InstrId: o.InstrID, TxId: o.TxID,
	}
	if o.Status == payments.StatusHeld {
		held := moneyJSON(o.Held)
		j.Held = &held
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
