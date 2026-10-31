// Package app holds the use cases of pricing: quoting what an operation costs and charging what was quoted
// (FR-28).
package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/pricing/internal/domain"
	"github.com/DrewGGM/aluna/internal/products"
)

// QuoteTTL is how long a quote can be used (FR-28): long enough for the customer to confirm, short enough
// that what it shows still holds.
const QuoteTTL = 5 * time.Minute

var (
	// ErrInvalidQuote means a quote that cannot be made: an operation that is not priced (only withdrawals and
	// transfers are), an amount that is not positive or not in the product's currency.
	ErrInvalidQuote = errors.New("invalid quote")
	// ErrQuoteUnavailable means a quote that does not exist for the account, the operation and the amount, was
	// already used or expired.
	ErrQuoteUnavailable = errors.New("the quote is not available")
)

// QuoteInput is an operation of Amount on deposit account AccountID of the entity, whose product version is
// ProductVersionID, to quote. Facts are what the product's rules read about the account and its party; the
// quote counts MonthTxCount itself.
type QuoteInput struct {
	LegalEntityID, AccountID, ProductVersionID ids.ID
	Operation                                  domain.Operation
	Amount                                     money.Amount
	Facts                                      products.RuleVars
}

// Quote is what an operation costs, shown before it is done and charged when it is (FR-28).
type Quote struct {
	ID, LegalEntityID, AccountID ids.ID
	Operation                    domain.Operation
	Amount                       money.Amount
	Fees                         []domain.Fee
	CreatedAt, ExpiresAt         time.Time
}

// QuoteUse is the use of quote QuoteID by an Operation of Amount on deposit account AccountID: the quote
// must be of that same operation.
type QuoteUse struct {
	QuoteID, AccountID ids.ID
	Operation          domain.Operation
	Amount             money.Amount
}

// Quotes is what quoting needs, within the transaction they run in.
type Quotes interface {
	Version(ctx context.Context, versionID ids.ID) (products.Version, error)
	Satisfies(v products.Version, rule string, vars products.RuleVars) bool
	// UsedThisMonth is how many quotes of the account of the entity were used in the current month
	// (America/Bogota).
	UsedThisMonth(ctx context.Context, entity, accountID ids.ID) (int, error)
	// InsertQuote stores q, which expires ttl after the database's now, and returns it with its times.
	InsertQuote(ctx context.Context, q Quote, ttl time.Duration) (Quote, error)
}

// MakeQuote quotes in: the fees of its operation by its product's rules, with their exemptions (CEL) decided
// on its facts and the account's quotes used this month (month_tx_count), valid for QuoteTTL.
func MakeQuote(ctx context.Context, s Quotes, in QuoteInput) (Quote, error) {
	v, err := s.Version(ctx, in.ProductVersionID)
	if err != nil {
		return Quote{}, err
	}
	c := v.Definition.Currency
	switch {
	case in.Operation != domain.Withdrawal && in.Operation != domain.Transfer:
		return Quote{}, fmt.Errorf("%w: operation %q", ErrInvalidQuote, in.Operation)
	case in.Amount.Sign() <= 0 || in.Amount.Currency() != c:
		return Quote{}, fmt.Errorf("%w: amount of %d units of %q, the product is in %s", ErrInvalidQuote, in.Amount.Units(),
			in.Amount.Currency().Code(), c.Code())
	}
	facts := in.Facts
	if facts.MonthTxCount, err = s.UsedThisMonth(ctx, in.LegalEntityID, in.AccountID); err != nil {
		return Quote{}, err
	}
	fees := domain.FeesFor(feeRules(v.Definition.Fees), in.Operation, c, func(rule string) bool {
		return s.Satisfies(v, rule, facts)
	})
	return s.InsertQuote(ctx, Quote{
		ID: ids.New(), LegalEntityID: in.LegalEntityID, AccountID: in.AccountID, Operation: in.Operation, Amount: in.Amount, Fees: fees,
	}, QuoteTTL)
}

func feeRules(fees []products.FeeRule) []domain.FeeRule {
	rules := make([]domain.FeeRule, len(fees))
	for i, f := range fees {
		rules[i] = domain.FeeRule{Code: f.Code, Operation: domain.Operation(f.Trigger), AmountUnits: f.AmountUnits, ExemptRule: f.ExemptRule}
	}
	return rules
}
