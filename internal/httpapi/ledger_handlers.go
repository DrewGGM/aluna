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

// Origen de los postings manuales que entran por POST /v1/transactions. Quedan guardados para siempre
// en ledger.transactions: rule_version "manual.v1" dice que los entries vinieron armados por quien
// llamó y no de una regla de posting versionada. Quién llamó queda en el registro de auditoría.
const (
	manualActor       = "api"
	manualChannel     = "api"
	manualRuleVersion = "manual.v1"
)

// Tipo de solicitud de maker-checker de una reversa (FR-3, FR-39) y el permiso que exige aprobarla.
const (
	reversalKind       = "ledger.reversal"
	reversalPermission = "ledger.transactions.reverse"
)

// LedgerHandlers implementa las operaciones del ledger del contrato (api/openapi.yaml).
type LedgerHandlers struct {
	ledger    *ledger.Service
	approvals *approvals.Service
	commands  commands
	reader    postgres.Querier
}

// NewLedgerHandlers crea los handlers del ledger y registra en a el tipo de solicitud de la reversa, que
// pasa por maker-checker. keys guarda las respuestas de los comandos con Idempotency-Key; reader sirve las
// lecturas fuera de transacción. La entidad legal de cada request es la de su token (FR-41).
func NewLedgerHandlers(l *ledger.Service, a *approvals.Service, uow *postgres.UnitOfWork, keys *idempotency.Store, reader postgres.Querier) *LedgerHandlers {
	a.RegisterKind(reversalKind, reversalPermission, reverseOnApproval(l))
	return &LedgerHandlers{ledger: l, approvals: a, commands: commands{uow: uow, keys: keys}, reader: reader}
}

// CreateLedgerAccount crea una cuenta de la entidad del token.
func (h *LedgerHandlers) CreateLedgerAccount(ctx context.Context, req gen.CreateLedgerAccountRequestObject) (gen.CreateLedgerAccountResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	cur, err := money.CurrencyOf(req.Body.Currency)
	if err != nil {
		return nil, fmt.Errorf("moneda de la cuenta: %w", err)
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

// GetLedgerAccount lee una cuenta de la entidad del token.
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

// GetLedgerAccountBalance lee el saldo de una cuenta de la entidad del token: el materializado o,
// con un corte de fecha valor o de conocimiento, el bitemporal.
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

// balanceCut lee los cortes del saldo bitemporal. Un parámetro vacío (?as_known_at=) llega como fecha
// cero y cuenta como ausente: con los dos ausentes se pide el materializado. El corte de conocimiento
// queda en UTC y en microsegundos, la precisión de posted_at.
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

// balanceAsOf es el saldo bitemporal (FR-8): devuelve los cortes con que se calculó y no lleva
// pendientes, que solo existen en el presente.
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

// PostTransaction asienta una transacción manual de la entidad del token, una sola vez por
// Idempotency-Key.
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

// GetTransaction lee una transacción de la entidad del token.
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

// reversalRequest es el payload de una solicitud de reversa: lo que el checker aprueba y lo que se asienta.
type reversalRequest struct {
	TransactionID ids.ID              `json:"transaction_id"`
	Reason        string              `json:"reason"`
	BusinessDate  *openapi_types.Date `json:"business_date,omitempty"`
}

// errReversalOfReversal es pedir la reversa de una transacción que ya es una reversa: aprobarla volvería a
// aplicar la original sin que el checker lo viera en el payload.
var errReversalOfReversal = errors.New("la transacción es una reversa: no se revierte")

// RequestReversal pide la reversa de una transacción de la entidad del token: queda como solicitud
// ledger.reversal hasta que otra persona la apruebe (FR-39), una sola vez por Idempotency-Key. La de una
// reversa se rechaza: para volver a aplicar la original se asienta de nuevo.
func (h *LedgerHandlers) RequestReversal(ctx context.Context, req gen.RequestReversalRequestObject) (gen.RequestReversalResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	id := ids.ID(req.Id)
	op := resourceOperation("transactions", id, "reversal", http.StatusAccepted)
	noteTargets(ctx, op.targets) // antes de validar el cuerpo: su rechazo también nombra la transacción
	if strings.TrimSpace(req.Body.Reason) == "" {
		return nil, fmt.Errorf("%w: falta el motivo de la reversa", ledger.ErrInvalidPosting)
	}
	payload := reversalRequest{TransactionID: id, Reason: req.Body.Reason, BusinessDate: req.Body.BusinessDate}
	resp, err := h.commands.idempotent(ctx, p, op, req.Params.IdempotencyKey, req.Body, func(tx postgres.Tx) (any, error) {
		original, err := h.ledger.Transaction(ctx, tx, p.LegalEntityID, id)
		if err != nil {
			return nil, err
		}
		if original.ReversesID != (ids.ID{}) {
			return nil, fmt.Errorf("%w: %s revierte a %s", errReversalOfReversal, id, original.ReversesID)
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

// reverseOnApproval es el Executor de reversalKind: asienta la reversa del payload en la entidad de quien
// aprueba, que queda como su actor. Un rechazo del ledger (un 4xx de su tabla de problems: ya revertida,
// periodo cerrado, fondos insuficientes) va envuelto en approvals.ErrCommandRejected y deja la solicitud
// failed; cualquier otro error sube.
func reverseOnApproval(l *ledger.Service) approvals.Executor {
	return func(ctx context.Context, tx postgres.Tx, payload json.RawMessage, checker access.Principal) (json.RawMessage, error) {
		var r reversalRequest
		if err := json.Unmarshal(payload, &r); err != nil {
			return nil, fmt.Errorf("payload de la reversa: %w", err)
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
			return nil, fmt.Errorf("resultado de la reversa: %w", err)
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

// requireBusinessDate rechaza la fecha cero (0001-01-01): el ledger la tomaría como ausente y pondría la
// de la entidad, y por la API la fecha de negocio la manda quien llama.
func requireBusinessDate(d openapi_types.Date) error {
	if d.IsZero() {
		return fmt.Errorf("%w: falta la fecha de negocio", ledger.ErrInvalidPosting)
	}
	return nil
}

// amountOf convierte un importe del contrato; una moneda desconocida es ErrUnknownCurrency.
func amountOf(m gen.Money) (money.Amount, error) {
	cur, err := money.CurrencyOf(m.Currency)
	if err != nil {
		return money.Amount{}, fmt.Errorf("moneda: %w", err)
	}
	return money.New(m.Amount, cur), nil
}

func moneyJSON(a money.Amount) gen.Money {
	return gen.Money{Amount: a.Units(), Currency: a.Currency().Code()}
}

// PlaceHold retiene fondos de una cuenta de la entidad del token, una sola vez por
// Idempotency-Key.
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

// CaptureHold asienta parte o todo un hold de la entidad del token, una sola vez por
// Idempotency-Key.
func (h *LedgerHandlers) CaptureHold(ctx context.Context, req gen.CaptureHoldRequestObject) (gen.CaptureHoldResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	op := resourceOperation("holds", ids.ID(req.Id), "capture", http.StatusOK)
	noteTargets(ctx, op.targets) // antes de validar el cuerpo: su rechazo también nombra el hold
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

// ReleaseHold libera un hold de la entidad del token, una sola vez por Idempotency-Key.
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
