// Package pricing is the pricing module (FR-28, FR-29): what an operation costs, its fees and its GMF, quoted
// before it is done and charged exactly when it is.
package pricing

import (
	"context"
	"errors"
	"fmt"

	"github.com/DrewGGM/aluna/internal/calendar"
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
	// GMF is the GMF of a debit: the part its exemption covers and the tax on the rest.
	GMF = domain.GMF
	// Charges are what an operation priced by a quote is charged: its fees and its GMF.
	Charges = app.Charges
	// Operation is an operation the products charge for.
	Operation = domain.Operation
	// Modality is how a party's GMF exemption applies to its accounts.
	Modality = app.Modality
	// ExemptionInput is a deposit account to exempt from GMF.
	ExemptionInput = app.ExemptionInput
	// Exemption is a deposit account exempt from GMF.
	Exemption = app.Exemption
)

// Operations with fees.
const (
	Withdrawal = domain.Withdrawal
	Transfer   = domain.Transfer
)

// Modalities of the GMF exemption: on a single account of the party, or split among its exempt accounts.
const (
	SingleAccount = app.SingleAccount
	SplitAccounts = app.SplitAccounts
)

var (
	// ErrInvalidQuote means a quote that cannot be made: an operation that is not priced (only withdrawals and
	// transfers are), an amount that is not positive or not in the product's currency.
	ErrInvalidQuote = app.ErrInvalidQuote
	// ErrQuoteUnavailable means a quote that does not exist for the account, the operation and the amount, was
	// already used or expired, or whose GMF exemption another operation used meanwhile.
	ErrQuoteUnavailable = app.ErrQuoteUnavailable
	// ErrGMFNotEligible means an account that cannot be exempt from GMF.
	ErrGMFNotEligible = app.ErrGMFNotEligible
	// ErrGMFExemptionTaken means an account already exempt or, with the exemption on a single account, a party
	// with another one.
	ErrGMFExemptionTaken = app.ErrGMFExemptionTaken
	// ErrUVTMissing means no UVT for the year of the business date, for an exempt account.
	ErrUVTMissing = app.ErrUVTMissing
	// ErrUnknownModality means a modality of the GMF exemption that is not one of the two.
	ErrUnknownModality = app.ErrUnknownModality
)

// ModalityOf is the modality named s: single_account or split_accounts.
func ModalityOf(s string) (Modality, error) {
	if m := Modality(s); m == SingleAccount || m == SplitAccounts {
		return m, nil
	}
	return "", fmt.Errorf("%w: %q", ErrUnknownModality, s)
}

// Total is what fees add up to, in currency c.
func Total(fees []Fee, c money.Currency) (money.Amount, error) { return domain.Total(fees, c) }

// Service prices the operations with the products' rules and the entity's GMF exemption modality.
type Service struct {
	products *products.Service
	calendar *calendar.Service
	modality Modality
}

// New creates the service; m is the entity's modality of the GMF exemption.
func New(p *products.Service, c *calendar.Service, m Modality) *Service {
	return &Service{products: p, calendar: c, modality: m}
}

// Quote quotes, within tx, an operation on a deposit account of the entity (FR-28): the fees of its product
// for the operation, with their exemptions decided on in.Facts, and its GMF (FR-29), with what remains of its
// party's exemption if the account is exempt; valid for 5 minutes. An operation that is not priced, or an
// amount that is not positive or not in the product's currency, is ErrInvalidQuote; an exempt account without
// a UVT for the year, ErrUVTMissing.
func (s *Service) Quote(ctx context.Context, tx postgres.Tx, in QuoteInput) (Quote, error) {
	p := s.ports(tx)
	return app.MakeQuote(ctx, p, p, in)
}

// ChargesFor marks, within tx, the quote of the entity that u names as used and returns what u's operation is
// charged: exactly what it quoted (FR-28), using the part of the party's GMF exemption it quoted. A quote
// used, expired, or of another account, operation or amount, or whose exemption another operation used
// meanwhile, is ErrQuoteUnavailable.
func (s *Service) ChargesFor(ctx context.Context, tx postgres.Tx, entity ids.ID, u QuoteUse) (Charges, error) {
	return app.Charge(ctx, s.ports(tx), entity, u)
}

// Charge prices, within tx, an operation that comes without a quote: it quotes it and uses the quote at once,
// so that it is recorded and counts, as any other, among the account's operations of the month.
func (s *Service) Charge(ctx context.Context, tx postgres.Tx, in QuoteInput) (Charges, error) {
	q, err := s.Quote(ctx, tx, in)
	if err != nil {
		return Charges{}, err
	}
	return s.ChargesFor(ctx, tx, in.LegalEntityID, QuoteUse{
		QuoteID: q.ID, AccountID: q.AccountID, PartyID: q.PartyID, Operation: q.Operation, Amount: q.Amount,
	})
}

// MarkExempt exempts, within tx, a deposit account from GMF (FR-29): its product must allow it and it must be
// in COP (ErrGMFNotEligible). With the exemption on a single account, a party with another exempt account is
// ErrGMFExemptionTaken, as is an account already exempt.
func (s *Service) MarkExempt(ctx context.Context, tx postgres.Tx, in ExemptionInput) (Exemption, error) {
	return app.MarkExempt(ctx, s.ports(tx), s.modality, in)
}

func (s *Service) ports(tx postgres.Tx) ports { return ports{Store: pg.NewStore(tx), s: s, tx: tx} }

// ports is the app's ports over tx; the store gives the quotes and the exemptions.
type ports struct {
	pg.Store
	s  *Service
	tx postgres.Tx
}

func (p ports) Version(ctx context.Context, versionID ids.ID) (products.Version, error) {
	return p.s.products.Version(ctx, p.tx, versionID)
}

func (p ports) Satisfies(v products.Version, rule string, vars products.RuleVars) bool {
	return p.s.products.Satisfies(v, rule, vars)
}

func (p ports) BusinessDate(ctx context.Context, entity ids.ID) (calendar.Date, error) {
	return p.s.calendar.BusinessDate(ctx, p.tx, entity)
}

// UVT is the UVT in force on day on; without one it is ErrUVTMissing, which has no problem (a 500 the client
// can retry once it is loaded), and not products.ErrNoMarketParameter, which is a 404.
func (p ports) UVT(ctx context.Context, on calendar.Date) (money.Precise, error) {
	uvt, err := p.s.products.MarketParameter(ctx, p.tx, products.UVT, "", on)
	if errors.Is(err, products.ErrNoMarketParameter) {
		return money.Precise{}, fmt.Errorf("%w: %d-%02d-%02d", ErrUVTMissing, on.Year, on.Month, on.Day)
	}
	return uvt, err
}
