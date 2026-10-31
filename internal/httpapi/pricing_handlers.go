package httpapi

import (
	"context"
	"fmt"
	"net/http"

	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/party"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/pricing"
	"github.com/DrewGGM/aluna/internal/products"
)

// PricingHandlers implements the contract's pricing operations (api/openapi.yaml). A quote needs facts of
// the account that pricing does not keep (its product, its party, its balance), so the handler gathers them
// from the modules that do.
type PricingHandlers struct {
	pricing  *pricing.Service
	deposits *deposits.Service
	parties  *party.Service
	ledger   *ledger.Service
	commands commands
}

// NewPricingHandlers creates the pricing handlers.
func NewPricingHandlers(p *pricing.Service, d *deposits.Service, parties *party.Service, l *ledger.Service, uow *postgres.UnitOfWork) *PricingHandlers {
	return &PricingHandlers{pricing: p, deposits: d, parties: parties, ledger: l, commands: commands{uow: uow}}
}

// CreateQuote quotes an operation on a deposit account of the token's entity (FR-28). It moves no money: it
// takes no Idempotency-Key, and a retry makes another quote.
func (h *PricingHandlers) CreateQuote(ctx context.Context, req gen.CreateQuoteRequestObject) (gen.CreateQuoteResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	account := ids.ID(req.Body.AccountId)
	op := operation{method: http.MethodPost, path: "/v1/quotes", success: http.StatusCreated, targets: []string{account.String()}}
	noteTargets(ctx, op.targets) // before validating the body: its rejection also names the account
	amount, err := amountOf(req.Body.Amount)
	if err != nil {
		return nil, fmt.Errorf("quote: %w", err)
	}
	resp, err := h.commands.run(ctx, op, func(tx postgres.Tx) (any, error) {
		in, err := h.quoteInput(ctx, tx, p.LegalEntityID, account, pricing.Operation(req.Body.Operation), amount)
		if err != nil {
			return nil, err
		}
		q, err := h.pricing.Quote(ctx, tx, in)
		if err != nil {
			return nil, err
		}
		return toQuoteJSON(q)
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// quoteInput is the quote of operation op of amount on deposit account id of the entity, with the facts its
// product's rules read: the kind and risk of its party and its balance.
func (h *PricingHandlers) quoteInput(ctx context.Context, tx postgres.Tx, entity, id ids.ID, op pricing.Operation, amount money.Amount,
) (pricing.QuoteInput, error) {
	a, err := h.deposits.Account(ctx, tx, entity, id)
	if err != nil {
		return pricing.QuoteInput{}, err
	}
	holder, err := h.parties.Get(ctx, tx, entity, a.PartyID)
	if err != nil {
		return pricing.QuoteInput{}, err
	}
	b, err := h.ledger.Balance(ctx, tx, a.LedgerAccountID)
	if err != nil {
		return pricing.QuoteInput{}, err
	}
	return pricing.QuoteInput{
		LegalEntityID: entity, AccountID: a.ID, ProductVersionID: a.ProductVersionID, Operation: op, Amount: amount,
		Facts: products.RuleVars{
			PartyKind: string(holder.Kind), RiskLevel: string(holder.Current.RiskLevel),
			BalanceUnits: ledger.NaturalBalance(ledger.KindLiability, b.Posted),
		},
	}, nil
}

func toQuoteJSON(q pricing.Quote) (gen.Quote, error) {
	fees := make([]gen.QuotedFee, len(q.Fees))
	for i, f := range q.Fees {
		fees[i] = gen.QuotedFee{Code: f.Code, Amount: moneyJSON(f.Amount)}
	}
	total, err := pricing.Total(q.Fees, q.Amount.Currency())
	if err != nil {
		return gen.Quote{}, err
	}
	return gen.Quote{
		Id: [16]byte(q.ID), AccountId: [16]byte(q.AccountID), Operation: gen.QuoteOperation(q.Operation),
		Amount: moneyJSON(q.Amount), Fees: fees, TotalFees: moneyJSON(total), ExpiresAt: q.ExpiresAt.UTC(),
	}, nil
}
