package app

import (
	"slices"
	"strings"

	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/pricing"
)

// Policy is how the entity treats the charges of a payment that does not go through (ADR-0016).
type Policy struct {
	// RefundFeesOnReturn refunds the fees of a payment the beneficiary's bank returned.
	RefundFeesOnReturn bool
	// AnnullingReasons are the ISO 20022 reasons of a return that annul the payment, which refund its GMF: the
	// transfer could not be completed. Another reason, a voluntary return, is a new operation and refunds none.
	AnnullingReasons []string
	// RestoreExemption restores to its party's GMF exemption what a payment rejected or annulled used of it,
	// while it is still that month.
	RestoreExemption bool
	// HonorQuotes charges a payment that waited for compliance the fees it was quoted when it was initiated;
	// otherwise it is charged the lower of those and the tariff in force when it is cleared. It is never charged
	// more than it was shown (Ley 1328 de 2009, art. 7 lit. p).
	HonorQuotes bool
}

// annuls reports whether a return for reason annuls the payment; the rail's code is compared as ISO 20022 writes
// it, in capitals and without spaces around it.
func (p Policy) annuls(reason string) bool {
	return slices.Contains(p.AnnullingReasons, strings.ToUpper(strings.TrimSpace(reason)))
}

// refunded are the charges of o a return gives back: its fees if the policy refunds them, and its GMF if the
// return annuls it.
func (p Policy) refunded(o Order, annulled bool) ([]pricing.Fee, money.Amount) {
	var fees []pricing.Fee
	if p.RefundFeesOnReturn {
		fees = o.Fees
	}
	if !annulled {
		return fees, money.New(0, o.Amount.Currency())
	}
	return fees, o.GMF
}

// charged are the fees of o, held now for current, the tariff in force: the tariff if o kept no quoted fees; its
// quoted fees if the policy honors quotes or they add up to less than the tariff; the tariff otherwise.
func (p Policy) charged(o Order, current []pricing.Fee) ([]pricing.Fee, error) {
	switch {
	case !o.Quoted:
		return current, nil
	case p.HonorQuotes:
		return o.QuotedFees, nil
	}
	quoted, err := pricing.Total(o.QuotedFees, o.Amount.Currency())
	if err != nil {
		return nil, err
	}
	now, err := pricing.Total(current, o.Amount.Currency())
	if err != nil {
		return nil, err
	}
	if quoted.Units() < now.Units() {
		return o.QuotedFees, nil
	}
	return current, nil
}

// restored is what of the GMF exemption o used goes back to its party when o did not debit its GMF after all
// (it was rejected, or annulled): all of it if the policy restores it, none otherwise.
func (p Policy) restored(o Order, debited bool) pricing.ExemptionUse {
	if debited || !p.RestoreExemption {
		return pricing.ExemptionUse{}
	}
	return o.GMFExemption
}
