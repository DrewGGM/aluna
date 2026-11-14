//go:build integration

package payments_test

import (
	"testing"

	"github.com/DrewGGM/aluna/internal/payments"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// settledOrder is a payment of units initiated, sent and accepted by the rail.
func (w world) settledOrder(t *testing.T, units int64) payments.Order {
	t.Helper()
	o := w.sentOrder(t, units)
	if err := w.take(t, w.rail.notification(t)); err != nil {
		t.Fatal(err)
	}
	return o
}

// returned takes the rail's word that o came back for reason.
func (w world) returned(t *testing.T, o payments.Order, reason string) {
	t.Helper()
	mustDo(t, func(tx postgres.Tx) error {
		return w.payments.TakeOutcome(t.Context(), tx, payments.RailOutcome{EndToEndID: o.EndToEndID, Status: payments.RailReturned, Reason: reason})
	})
}

// TestAReturnRefundsTheChargesOfAnAnnulledPayment (FR-31, ADR-0016): a payment of the tiered product (a fee of
// 3,000 COP and its GMF) that comes back because the account does not exist (AC01) was annulled: the customer
// gets back its amount, its fee and its GMF. One that comes back by the customer's request (CUST) is a new
// operation: the amount and the fee come back, not the GMF.
func TestAReturnRefundsTheChargesOfAnAnnulledPayment(t *testing.T) {
	for reason, gmfBack := range map[string]bool{"AC01": true, "CUST": false} {
		w := worldOf(t, "valid-tiered-savings.json", true)
		before := w.balance(t).Posted.Units()
		o := w.settledOrder(t, 30000000)

		w.returned(t, o, reason)

		want := before + o.GMF.Units() // a deposit is a liability: its balance is a credit, negative
		if gmfBack {
			want = before
		}
		if got := w.balance(t).Posted.Units(); got != want {
			t.Errorf("%s: the account has %d after the return, want %d (the GMF %v back: %t)", reason, got, want, o.GMF, gmfBack)
		}
	}
}

// TestARejectedPaymentRestoresItsExemption (ADR-0016): a payment from an account exempt from GMF uses part of its
// party's exemption of the month when it is held; if the rail rejects it, it never debited anything, and that
// part is restored.
func TestARejectedPaymentRestoresItsExemption(t *testing.T) {
	w := newWorld(t, true)
	exemptAccount(t, w)
	o := w.sentOrder(t, 5001300) // an amount ending in 13 pesos, which the simulated rail rejects
	if used := w.exemptUsed(t); used != o.Amount.Units() || o.GMFExemption.Amount != o.Amount {
		t.Fatalf("held, the order used %v and the party %d of the exemption, want its amount", o.GMFExemption, used)
	}

	if err := w.take(t, w.rail.notification(t)); err != nil {
		t.Fatal(err)
	}

	if used := w.exemptUsed(t); used != 0 {
		t.Fatalf("after the rejection the party used %d of the exemption, want none", used)
	}
}

// TestAnAnnulledPaymentRestoresItsExemption (ADR-0016): a payment from an exempt account that comes back because
// the account does not exist (AC01) was annulled: what it used of its party's exemption is restored, under the
// account's lock taken before the ledger's.
func TestAnAnnulledPaymentRestoresItsExemption(t *testing.T) {
	w := newWorld(t, true)
	exemptAccount(t, w)
	o := w.settledOrder(t, 5000000)

	w.returned(t, o, "AC01")

	if used := w.exemptUsed(t); used != 0 {
		t.Fatalf("after the return the party used %d of the exemption, want none", used)
	}
}

// exemptUsed is what the party of the world's account uses of its GMF exemption, in all months.
func (w world) exemptUsed(t *testing.T) int64 {
	t.Helper()
	var used int64
	if err := testPool.QueryRow(t.Context(), `SELECT coalesce(sum(exempt_used_units - restored_units), 0) FROM pricing.gmf_monthly
		WHERE legal_entity_id = $1 AND party_id = $2`, w.entity, w.account.PartyID).Scan(&used); err != nil {
		t.Fatal(err)
	}
	return used
}
