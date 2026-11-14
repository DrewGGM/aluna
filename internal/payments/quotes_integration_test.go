//go:build integration

package payments_test

import (
	"errors"
	"testing"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/payments"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/pricing"
)

// quoteOf stores, as the tables' owner, a quote of a transfer of units from the world's account with a fee of fee
// units, one the tariff did not make, so that the two differ: valid for 5 minutes, or expired a minute ago.
func (w world) quoteOf(t *testing.T, units, fee int64, expired bool) ids.ID {
	t.Helper()
	id, from := ids.New(), "0 minutes"
	if expired {
		from = "-6 minutes"
	}
	if err := asOwner(t, `INSERT INTO pricing.quotes (id, legal_entity_id, account_id, operation, currency, amount_units, fees, created_at,
			expires_at)
		VALUES ($1, $2, $3, 'transfer', 'COP', $4, jsonb_build_array(jsonb_build_object('code', 'transfer_fee', 'units', $5::bigint)),
			statement_timestamp() + $6::interval, statement_timestamp() + $6::interval + interval '5 minutes')`,
		id, w.entity, w.account.ID, units, fee, from); err != nil {
		t.Fatal(err)
	}
	return id
}

// storedFees are the fees order id keeps: those it holds, and those it was quoted to wait for compliance.
func storedFees(t *testing.T, id ids.ID) (held, quoted string) {
	t.Helper()
	if err := testPool.QueryRow(t.Context(), `SELECT fees::text, coalesce(quoted_fees::text, '') FROM payments.orders WHERE id = $1`, id).
		Scan(&held, &quoted); err != nil {
		t.Fatal(err)
	}
	return held, quoted
}

// TestAClearedPaymentIsChargedItsLowerQuotedFee (ADR-0016): a payment of the tiered product (3,000 COP a transfer)
// that waits for compliance with a quote of a 1,000 fee keeps that fee without using the quote, and once cleared is
// charged it, the lower of the two: never more than it was shown.
func TestAClearedPaymentIsChargedItsLowerQuotedFee(t *testing.T) {
	w := worldOf(t, "valid-tiered-savings.json", true)
	in := w.input(t, 20000000, "match-sanctioned")
	in.QuoteID = w.quoteOf(t, 20000000, 100000, false)
	o, err := w.initiate(t, in)
	if err != nil || o.Status != payments.StatusPendingScreening {
		t.Fatalf("a payment to a possible match = %+v (%v), want it pending", o, err)
	}
	var used bool
	if err := testPool.QueryRow(t.Context(), `SELECT used_at IS NOT NULL FROM pricing.quotes WHERE id = $1`, in.QuoteID).Scan(&used); err != nil || used {
		t.Fatalf("the quote is used: %t (%v), want it free while the payment waits", used, err)
	}

	mustDo(t, func(tx postgres.Tx) error { return w.payments.ResolveScreening(t.Context(), tx, w.entity, o.ID, true) })

	want := `[{"code": "transfer_fee", "units": 100000}]`
	if held, quoted := storedFees(t, o.ID); held != want || quoted != want {
		t.Fatalf("the cleared payment holds %s and was quoted %s, want both %s", held, quoted, want)
	}
}

// TestAWaitingPaymentNeedsAQuoteItCanUse (FR-28, ADR-0016): a payment that will wait for compliance checks its quote
// when it is initiated, as one held at once: one that expired, or of another amount, is ErrQuoteUnavailable.
// Without a quote it keeps no fees: the core showed it none, and it is charged as it is cleared.
func TestAWaitingPaymentNeedsAQuoteItCanUse(t *testing.T) {
	w := worldOf(t, "valid-tiered-savings.json", true)
	for name, quote := range map[string]ids.ID{
		"another amount": w.quoteOf(t, 20000000, 100000, false), "expired": w.quoteOf(t, 30000000, 100000, true),
	} {
		in := w.input(t, 30000000, "match-sanctioned")
		in.QuoteID = quote
		if _, err := w.initiate(t, in); !errors.Is(err, pricing.ErrQuoteUnavailable) {
			t.Errorf("waiting with a quote of %s = %v, want ErrQuoteUnavailable", name, err)
		}
	}

	o, err := w.initiate(t, w.input(t, 30000000, "match-sanctioned"))
	if err != nil {
		t.Fatal(err)
	}

	if _, quoted := storedFees(t, o.ID); quoted != "" {
		t.Fatalf("waiting without a quote kept %s, want no fees", quoted)
	}
}
