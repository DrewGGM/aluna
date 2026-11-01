package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/pricing/internal/domain"
	"github.com/DrewGGM/aluna/internal/products"
)

var (
	// ErrGMFNotEligible means an account that cannot be exempt from GMF: its product does not allow it, it is
	// not in COP or it is closed.
	ErrGMFNotEligible = errors.New("the account cannot be exempt from GMF")
	// ErrGMFExemptionTaken means, with the exemption on a single account, a party that already has another
	// account exempt; or an account already exempt.
	ErrGMFExemptionTaken = errors.New("the GMF exemption is already on an account of the party")
	// ErrUnknownModality means a modality of the GMF exemption that is not one of the two.
	ErrUnknownModality = errors.New("unknown modality of the GMF exemption")
	// ErrUVTMissing means no UVT for the year of the business date, for an exempt account: the market
	// parameters are missing, not the request.
	ErrUVTMissing = errors.New("no UVT is loaded for the year of the business date")
)

// Modality is how a party's GMF exemption applies to its accounts (FR-29).
type Modality string

// Modalities of the GMF exemption: on a single account of the party, or split among its exempt accounts.
const (
	SingleAccount Modality = "single_account"
	SplitAccounts Modality = "split_accounts"
)

// ExemptionInput is deposit account AccountID of the entity, of party PartyID, with product version
// ProductVersionID and in Currency, to exempt from GMF.
type ExemptionInput struct {
	LegalEntityID, AccountID, PartyID, ProductVersionID ids.ID
	Currency                                            money.Currency
}

// Exemption is a deposit account exempt from GMF.
type Exemption struct {
	AccountID, PartyID ids.ID
	MarkedAt           time.Time
}

// Exempting is what exempting an account needs, within the transaction it runs in.
type Exempting interface {
	Version(ctx context.Context, versionID ids.ID) (products.Version, error)
	// LockParty serializes, until the transaction ends, the exemptions of party of the entity.
	LockParty(ctx context.Context, entity, party ids.ID) error
	// ExemptAccounts are the party's accounts exempt from GMF.
	ExemptAccounts(ctx context.Context, entity, party ids.ID) ([]ids.ID, error)
	// InsertExemption stores in and returns it with its time.
	InsertExemption(ctx context.Context, in ExemptionInput) (Exemption, error)
}

// MarkExempt exempts an account from GMF (FR-29): its product must allow it and it must be in COP
// (ErrGMFNotEligible). With m SingleAccount the party can have only one exempt account; with SplitAccounts,
// several, which share the exemption. An account already exempt is ErrGMFExemptionTaken.
func MarkExempt(ctx context.Context, s Exempting, m Modality, in ExemptionInput) (Exemption, error) {
	v, err := s.Version(ctx, in.ProductVersionID)
	if err != nil {
		return Exemption{}, err
	}
	if !v.Definition.GMFExemptEligible || !domain.Levied(in.Currency) {
		return Exemption{}, fmt.Errorf("%w: account %s of a product that does not allow it, or not in COP", ErrGMFNotEligible, in.AccountID)
	}
	if err := s.LockParty(ctx, in.LegalEntityID, in.PartyID); err != nil {
		return Exemption{}, err
	}
	exempt, err := s.ExemptAccounts(ctx, in.LegalEntityID, in.PartyID)
	if err != nil {
		return Exemption{}, err
	}
	for _, id := range exempt {
		if id == in.AccountID || m == SingleAccount {
			return Exemption{}, fmt.Errorf("%w: account %s of party %s is exempt", ErrGMFExemptionTaken, id, in.PartyID)
		}
	}
	return s.InsertExemption(ctx, in)
}

// Exemptions is what the exemption of the entity's current month needs.
type Exemptions interface {
	BusinessDate(ctx context.Context, entity ids.ID) (calendar.Date, error)
	// UVT is the UVT in pesos in force on day on, or ErrUVTMissing.
	UVT(ctx context.Context, on calendar.Date) (money.Precise, error)
}

// GMFs is what the GMF of an operation needs, within the transaction it runs in.
type GMFs interface {
	Exemptions
	// IsExempt reports whether account accountID of the entity is exempt from GMF.
	IsExempt(ctx context.Context, entity, accountID ids.ID) (bool, error)
	// ExemptUsed is what party's exempt debits used of the exemption of month, in COP minor units, locked
	// (FOR UPDATE) until the transaction ends.
	ExemptUsed(ctx context.Context, entity, party ids.ID, month calendar.Date) (int64, error)
}

// exemptionOf is the exemption of the entity's current month, by its business date: the month's first day and
// its cap, 350 times the UVT of the year (the one in force on its January 1), so that it does not change
// within a month.
func exemptionOf(ctx context.Context, g Exemptions, entity ids.ID) (calendar.Date, money.Amount, error) {
	today, err := g.BusinessDate(ctx, entity)
	if err != nil {
		return calendar.Date{}, money.Amount{}, err
	}
	uvt, err := g.UVT(ctx, calendar.Date{Year: today.Year, Month: time.January, Day: 1})
	if err != nil {
		return calendar.Date{}, money.Amount{}, err
	}
	limit, err := domain.ExemptionCap(uvt)
	if err != nil {
		return calendar.Date{}, money.Amount{}, err
	}
	return calendar.Date{Year: today.Year, Month: today.Month, Day: 1}, limit, nil
}

// gmfOf is the GMF of in's operation: an account exempt from it uses what remains of its party's exemption
// this month, which stays locked until the transaction ends.
func gmfOf(ctx context.Context, g GMFs, in QuoteInput) (domain.GMF, error) {
	remaining := money.New(0, in.Amount.Currency())
	exempt, err := g.IsExempt(ctx, in.LegalEntityID, in.AccountID)
	if err != nil {
		return domain.GMF{}, err
	}
	if exempt {
		month, limit, err := exemptionOf(ctx, g, in.LegalEntityID)
		if err != nil {
			return domain.GMF{}, err
		}
		used, err := g.ExemptUsed(ctx, in.LegalEntityID, in.PartyID, month)
		if err != nil {
			return domain.GMF{}, err
		}
		remaining = money.New(limit.Units()-used, limit.Currency())
	}
	return domain.GMFOf(in.Amount, remaining)
}

// Charges are what an operation priced by a quote is charged: its fees and its GMF (FR-28, FR-29).
type Charges struct {
	Fees []domain.Fee
	GMF  money.Amount
}

// Charging is what charging a quote needs, within the transaction it runs in.
type Charging interface {
	Exemptions
	// UseExemption adds units to what party used of the exemption of month, unless that would pass limit:
	// then it is ErrQuoteUnavailable.
	UseExemption(ctx context.Context, entity, party ids.ID, month calendar.Date, units, limit int64) error
	// UseQuote marks as used the quote of the entity that u names, if it is of u's account, operation and
	// amount, is not used and has not expired, and returns it; otherwise it is ErrQuoteUnavailable.
	UseQuote(ctx context.Context, entity ids.ID, u QuoteUse) (Quote, error)
}

// Charge marks the quote u names as used and returns exactly what it quoted (FR-28). The exemption it uses is
// taken from its party's of the current month: if another operation used it meanwhile, so that it no longer
// covers the quote's, the quote is ErrQuoteUnavailable and the customer quotes again.
func Charge(ctx context.Context, s Charging, entity ids.ID, u QuoteUse) (Charges, error) {
	q, err := s.UseQuote(ctx, entity, u)
	if err != nil {
		return Charges{}, err
	}
	if q.GMF.Exempt.Sign() > 0 {
		month, limit, err := exemptionOf(ctx, s, entity)
		if err != nil {
			return Charges{}, err
		}
		if err := s.UseExemption(ctx, entity, u.PartyID, month, q.GMF.Exempt.Units(), limit.Units()); err != nil {
			return Charges{}, err
		}
	}
	return Charges{Fees: q.Fees, GMF: q.GMF.Tax}, nil
}
