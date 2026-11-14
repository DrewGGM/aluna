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

// restored is what of the GMF exemption o used goes back to its party when o did not debit its GMF after all
// (it was rejected, or annulled): all of it if the policy restores it, none otherwise.
func (p Policy) restored(o Order, debited bool) pricing.ExemptionUse {
	if debited || !p.RestoreExemption {
		return pricing.ExemptionUse{}
	}
	return o.GMFExemption
}
