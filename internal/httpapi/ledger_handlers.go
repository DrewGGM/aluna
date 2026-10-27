package httpapi

import (
	"context"
	"fmt"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// Origen de los postings manuales que entran por POST /v1/transactions. Quedan guardados para siempre
// en ledger.transactions: rule_version "manual.v1" dice que los entries vinieron armados por quien
// llamó y no de una regla de posting versionada. Con autenticación, el actor será el del token (T-035).
const (
	manualActor       = "api"
	manualChannel     = "api"
	manualRuleVersion = "manual.v1"
)

// LedgerHandlers implementa las operaciones del ledger del contrato (api/openapi.yaml).
type LedgerHandlers struct {
	ledger   *ledger.Service
	uow      *postgres.UnitOfWork
	commands idempotentCommands
	reader   postgres.Querier
	// entity es la entidad legal del despliegue (un despliegue por entidad, ADR-0002); con
	// autenticación vendrá del token (T-035).
	entity ids.ID
}

// NewLedgerHandlers crea los handlers del ledger. keys guarda las respuestas de los comandos con
// Idempotency-Key; reader sirve las lecturas fuera de transacción.
func NewLedgerHandlers(l *ledger.Service, uow *postgres.UnitOfWork, keys *idempotency.Store, reader postgres.Querier, entity ids.ID) *LedgerHandlers {
	return &LedgerHandlers{
		ledger: l, uow: uow, commands: idempotentCommands{uow: uow, keys: keys, entity: entity}, reader: reader, entity: entity,
	}
}

// CreateLedgerAccount crea una cuenta de la entidad del despliegue.
func (h *LedgerHandlers) CreateLedgerAccount(ctx context.Context, req gen.CreateLedgerAccountRequestObject) (gen.CreateLedgerAccountResponseObject, error) {
	cur, err := money.CurrencyOf(req.Body.Currency)
	if err != nil {
		return nil, fmt.Errorf("moneda de la cuenta: %w", err)
	}
	in := ledger.CreateAccountInput{
		LegalEntityID: h.entity, Code: req.Body.Code, Kind: ledger.Kind(req.Body.Kind), Currency: cur,
		Mode: ledger.BalanceMode(req.Body.BalanceMode),
	}
	if req.Body.LowerLimit != nil {
		in.LowerLimit = *req.Body.LowerLimit
	}
	var account ledger.Account
	err = h.uow.Do(ctx, func(tx postgres.Tx) error {
		var err error
		account, err = h.ledger.CreateAccount(ctx, tx, in)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.CreateLedgerAccount201JSONResponse(toAccountJSON(account)), nil
}

// GetLedgerAccount lee una cuenta de la entidad del despliegue.
func (h *LedgerHandlers) GetLedgerAccount(ctx context.Context, req gen.GetLedgerAccountRequestObject) (gen.GetLedgerAccountResponseObject, error) {
	account, err := h.ledger.Account(ctx, h.reader, h.entity, ids.ID(req.Id))
	if err != nil {
		return nil, err
	}
	return gen.GetLedgerAccount200JSONResponse(toAccountJSON(account)), nil
}

// GetLedgerAccountBalance lee el saldo materializado, con signo y en la naturaleza de la cuenta.
func (h *LedgerHandlers) GetLedgerAccountBalance(ctx context.Context, req gen.GetLedgerAccountBalanceRequestObject) (gen.GetLedgerAccountBalanceResponseObject, error) {
	account, err := h.ledger.Account(ctx, h.reader, h.entity, ids.ID(req.Id))
	if err != nil {
		return nil, err
	}
	b, err := h.ledger.Balance(ctx, h.reader, account.ID)
	if err != nil {
		return nil, err
	}
	cur := account.Currency.Code()
	return gen.GetLedgerAccountBalance200JSONResponse{
		AccountId:      req.Id,
		Posted:         gen.Money{Amount: b.Posted, Currency: cur},
		Natural:        gen.Money{Amount: ledger.NaturalBalance(account.Kind, b.Posted), Currency: cur},
		PendingDebits:  gen.Money{Amount: b.PendingDebits, Currency: cur},
		PendingCredits: gen.Money{Amount: b.PendingCredits, Currency: cur},
		Version:        b.Version,
	}, nil
}

// PostTransaction asienta una transacción manual de la entidad del despliegue, una sola vez por
// Idempotency-Key.
func (h *LedgerHandlers) PostTransaction(ctx context.Context, req gen.PostTransactionRequestObject) (gen.PostTransactionResponseObject, error) {
	entries := make([]ledger.EntryInput, len(req.Body.Entries))
	for i, e := range req.Body.Entries {
		cur, err := money.CurrencyOf(e.Amount.Currency)
		if err != nil {
			return nil, fmt.Errorf("moneda del entry %d: %w", i, err)
		}
		entries[i] = ledger.EntryInput{AccountID: ids.ID(e.AccountId), Amount: money.New(e.Amount.Amount, cur)}
	}
	in := ledger.PostInput{
		LegalEntityID: h.entity, Code: req.Body.Code, ValueDate: req.Body.ValueDate.Time,
		BusinessDate: req.Body.BusinessDate.Time, Source: req.Body.Source, ExternalRef: req.Body.ExternalRef,
		Actor: manualActor, Channel: manualChannel, RuleVersion: manualRuleVersion, Entries: entries,
	}
	resp, err := h.commands.run(ctx, postTransaction, req.Params.IdempotencyKey, req.Body, func(tx postgres.Tx) (any, error) {
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

// GetTransaction lee una transacción de la entidad del despliegue.
func (h *LedgerHandlers) GetTransaction(ctx context.Context, req gen.GetTransactionRequestObject) (gen.GetTransactionResponseObject, error) {
	tx, err := h.ledger.Transaction(ctx, h.reader, h.entity, ids.ID(req.Id))
	if err != nil {
		return nil, err
	}
	return gen.GetTransaction200JSONResponse(toTransactionJSON(tx)), nil
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
		entries[i] = gen.Entry{
			AccountId: [16]byte(e.AccountID),
			Amount:    gen.Money{Amount: e.Amount.Units(), Currency: e.Amount.Currency().Code()},
		}
	}
	return gen.Transaction{
		Id: [16]byte(tx.ID), Code: tx.Code, ValueDate: dateOf(tx.ValueDate), BusinessDate: dateOf(tx.BusinessDate),
		PostedAt: tx.PostedAt, Source: tx.Source, ExternalRef: tx.ExternalRef, Entries: entries,
	}
}

func dateOf(t time.Time) openapi_types.Date { return openapi_types.Date{Time: t} }
