package deposits

import (
	"context"
	"fmt"

	pg "github.com/DrewGGM/aluna/internal/deposits/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/pricing"
	"github.com/DrewGGM/aluna/internal/products"
)

// QuoteInput is, within tx, the quote of op of amount on deposit account id of the entity, with the facts its
// product's rules read (FR-28): the kind and risk of its party and its balance. An unknown account is
// ErrAccountNotFound.
func (s *Service) QuoteInput(ctx context.Context, tx postgres.Tx, entity, id ids.ID, op pricing.Operation, amount money.Amount,
) (pricing.QuoteInput, error) {
	a, err := s.Account(ctx, tx, entity, id)
	if err != nil {
		return pricing.QuoteInput{}, err
	}
	return s.quoteInput(ctx, tx, a, op, amount)
}

func (s *Service) quoteInput(ctx context.Context, tx postgres.Tx, a Account, op pricing.Operation, amount money.Amount) (pricing.QuoteInput, error) {
	holder, err := s.parties.Get(ctx, tx, a.LegalEntityID, a.PartyID)
	if err != nil {
		return pricing.QuoteInput{}, err
	}
	b, err := s.ledger.Balance(ctx, tx, a.LedgerAccountID)
	if err != nil {
		return pricing.QuoteInput{}, err
	}
	return pricing.QuoteInput{
		LegalEntityID: a.LegalEntityID, AccountID: a.ID, PartyID: a.PartyID, ProductVersionID: a.ProductVersionID, Operation: op,
		Amount: amount,
		Facts: products.RuleVars{
			PartyKind: string(holder.Kind), RiskLevel: string(holder.Current.RiskLevel),
			BalanceUnits: ledger.NaturalBalance(ledger.KindLiability, b.Posted),
		},
	}, nil
}

// MarkGMFExempt exempts, within tx, deposit account id of the entity from GMF (FR-29), as pricing.MarkExempt:
// a closed account is pricing.ErrGMFNotEligible and an unknown one ErrAccountNotFound. It locks the account,
// as every change of its status does, so that it does not close while it is exempted.
func (s *Service) MarkGMFExempt(ctx context.Context, tx postgres.Tx, entity, id ids.ID) (pricing.Exemption, error) {
	a, err := pg.NewStore(tx).LockAccount(ctx, entity, id)
	if err != nil {
		return pricing.Exemption{}, err
	}
	if a.Status == Closed {
		return pricing.Exemption{}, fmt.Errorf("%w: account %s is closed", pricing.ErrGMFNotEligible, id)
	}
	return s.pricing.MarkExempt(ctx, tx, pricing.ExemptionInput{
		LegalEntityID: entity, AccountID: a.ID, PartyID: a.PartyID, ProductVersionID: a.ProductVersionID, Currency: a.Currency,
	})
}

// Charges are what op of amount on account a pays (FR-28, FR-29): what quote quoteID quoted or, with the zero
// ID, what it costs now.
func (m movements) Charges(ctx context.Context, a Account, op pricing.Operation, amount money.Amount, quoteID ids.ID) (pricing.Charges, error) {
	if quoteID != (ids.ID{}) {
		return m.s.pricing.ChargesFor(ctx, m.tx, a.LegalEntityID, pricing.QuoteUse{
			QuoteID: quoteID, AccountID: a.ID, PartyID: a.PartyID, Operation: op, Amount: amount,
		})
	}
	in, err := m.s.quoteInput(ctx, m.tx, a, op, amount)
	if err != nil {
		return pricing.Charges{}, err
	}
	return m.s.pricing.Charge(ctx, m.tx, in)
}

// FeeIncome is the ledger account the product of a names for its fees.
func (m movements) FeeIncome(ctx context.Context, a Account) (ids.ID, error) {
	return m.productAccountOf(ctx, a, "fee income", func(v products.Version) string { return v.Definition.Accounts.FeeIncome })
}

// GMFPayable is the ledger account the product of a names for the GMF.
func (m movements) GMFPayable(ctx context.Context, a Account) (ids.ID, error) {
	return m.productAccountOf(ctx, a, "GMF payable", func(v products.Version) string { return v.Definition.Accounts.GMFPayable })
}
