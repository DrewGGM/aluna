package httpapi

import (
	"context"
	"fmt"
	"net/http"

	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/pricing"
)

// PricingHandlers implements the contract's pricing operations (api/openapi.yaml). Deposits knows the
// account a quote or an exemption is of, so the handlers go through it.
type PricingHandlers struct {
	pricing  *pricing.Service
	deposits *deposits.Service
	commands commands
}

// NewPricingHandlers creates the pricing handlers.
func NewPricingHandlers(p *pricing.Service, d *deposits.Service, uow *postgres.UnitOfWork) *PricingHandlers {
	return &PricingHandlers{pricing: p, deposits: d, commands: commands{uow: uow}}
}

// CreateQuote quotes an operation on a deposit account of the token's entity (FR-28, FR-29). It moves no
// money: it takes no Idempotency-Key, and a retry makes another quote.
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
		in, err := h.deposits.QuoteInput(ctx, tx, p.LegalEntityID, account, pricing.Operation(req.Body.Operation), amount)
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
		Amount: moneyJSON(q.Amount), Fees: fees, TotalFees: moneyJSON(total), Gmf: moneyJSON(q.GMF.Tax), ExpiresAt: q.ExpiresAt.UTC(),
	}, nil
}

// CreateGMFExemption exempts a deposit account of the token's entity from GMF (FR-29). It moves no money, so
// it takes no Idempotency-Key: a retry finds the account exempt.
func (h *PricingHandlers) CreateGMFExemption(ctx context.Context, req gen.CreateGMFExemptionRequestObject) (gen.CreateGMFExemptionResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	op := resourceOperation("deposit-accounts", ids.ID(req.Id), "gmf-exemption", http.StatusCreated)
	resp, err := h.commands.run(ctx, op, func(tx postgres.Tx) (any, error) {
		e, err := h.deposits.MarkGMFExempt(ctx, tx, p.LegalEntityID, ids.ID(req.Id))
		if err != nil {
			return nil, err
		}
		return gen.GMFExemption{AccountId: [16]byte(e.AccountID), PartyId: [16]byte(e.PartyID), MarkedAt: e.MarkedAt.UTC()}, nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}
