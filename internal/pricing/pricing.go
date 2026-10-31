// Package pricing is the pricing module (FR-28, FR-29): what an operation costs, quoted before it is done and
// charged exactly when it is.
package pricing

import (
	"context"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
	pg "github.com/DrewGGM/aluna/internal/pricing/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/pricing/internal/app"
	"github.com/DrewGGM/aluna/internal/pricing/internal/domain"
	"github.com/DrewGGM/aluna/internal/products"
)

type (
	// QuoteInput is an operation on a deposit account to quote.
	QuoteInput = app.QuoteInput
	// QuoteUse is the use of a quote by the operation it quoted.
	QuoteUse = app.QuoteUse
	// Quote is what an operation costs, shown before it is done.
	Quote = app.Quote
	// Fee is a fee an operation pays.
	Fee = domain.Fee
	// Operation is an operation the products charge for.
	Operation = domain.Operation
)

// Operations with fees.
const (
	Withdrawal = domain.Withdrawal
	Transfer   = domain.Transfer
)

var (
	// ErrInvalidQuote means a quote that cannot be made: an operation that is not priced (only withdrawals and
	// transfers are), an amount that is not positive or not in the product's currency.
	ErrInvalidQuote = app.ErrInvalidQuote
	// ErrQuoteUnavailable means a quote that does not exist for the account, the operation and the amount, was
	// already used or expired.
	ErrQuoteUnavailable = app.ErrQuoteUnavailable
)

// Total is what fees add up to, in currency c.
func Total(fees []Fee, c money.Currency) (money.Amount, error) { return domain.Total(fees, c) }

// Charges are what an operation priced by a quote is charged: its fees and its GMF (FR-29).
type Charges struct {
	Fees []Fee
	GMF  money.Amount
}

// Service prices the operations with the products' rules.
type Service struct{ products *products.Service }

// New creates the service.
func New(p *products.Service) *Service { return &Service{products: p} }

// Quote quotes, within tx, an operation on a deposit account of the entity (FR-28): the fees of its product
// for the operation, with their exemptions decided on in.Facts, valid for 5 minutes. An operation that is not
// priced, or an amount that is not positive or not in the product's currency, is ErrInvalidQuote.
func (s *Service) Quote(ctx context.Context, tx postgres.Tx, in QuoteInput) (Quote, error) {
	return app.MakeQuote(ctx, quotes{Store: pg.NewStore(tx), s: s, tx: tx}, in)
}

// ChargesFor marks, within tx, the quote of the entity that u names as used and returns what u's operation is
// charged: exactly what it quoted (FR-28). A quote used, expired, or of another account, operation or amount
// is ErrQuoteUnavailable.
func (s *Service) ChargesFor(ctx context.Context, tx postgres.Tx, entity ids.ID, u QuoteUse) (Charges, error) {
	fees, err := pg.NewStore(tx).UseQuote(ctx, entity, u)
	if err != nil {
		return Charges{}, err
	}
	return Charges{Fees: fees, GMF: money.New(0, u.Amount.Currency())}, nil // no GMF is priced yet (FR-29)
}

// quotes is app.Quotes over tx; the store gives UsedThisMonth and InsertQuote.
type quotes struct {
	pg.Store
	s  *Service
	tx postgres.Tx
}

func (q quotes) Version(ctx context.Context, versionID ids.ID) (products.Version, error) {
	return q.s.products.Version(ctx, q.tx, versionID)
}

func (q quotes) Satisfies(v products.Version, rule string, vars products.RuleVars) bool {
	return q.s.products.Satisfies(v, rule, vars)
}
