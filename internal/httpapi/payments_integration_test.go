//go:build integration

package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/party"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// payingWorld is a depositWorld with the simulated rail's clearing account and an account funded with
// 1,000,000 COP.
func payingWorld(t *testing.T) (depositWorld, gen.DepositAccount) {
	t.Helper()
	w := newDepositWorld(t)
	if status := call(t, w.ts, http.MethodPost, "/v1/ledger-accounts",
		`{"code":"L.CLEARING.SIMBREB","kind":"liability","currency":"COP","balance_mode":"async"}`, nil); status != http.StatusCreated {
		t.Fatalf("create the clearing account = %d", status)
	}
	a := w.opened(t, w.party(t, party.KYCApproved), w.savings(t, true))
	expectCreated(t, "deposit", w.move(t, a.Id.String(), "deposits", amountBody(100000000)))
	return w, a
}

func paymentBody(a gen.DepositAccount, units int64, rail, key string) string {
	return `{"account_id":"` + a.Id.String() + `","amount":{"amount":` + strconv.FormatInt(units, 10) + `,"currency":"COP"},"rail":"` +
		rail + `","creditor":{"key":"` + key + `"}}`
}

// paid decodes the order of r, which must be 202.
func paid(t *testing.T, r response) gen.PaymentOrder {
	t.Helper()
	var o gen.PaymentOrder
	if err := json.Unmarshal(r.body, &o); err != nil || r.status != http.StatusAccepted {
		t.Fatalf("pay = %d %s (%v), want 202", r.status, r.body, err)
	}
	return o
}

// TestAPaymentOverHTTP (FR-31, US-8): a payment of 300,000 COP to a beneficiary screening clears is 202 held,
// holding 301,200 with its GMF; the retry with the same key gets the same answer, and the account's balance
// is untouched until it settles.
func TestAPaymentOverHTTP(t *testing.T) {
	w, a := payingWorld(t)
	key := ids.New().String()

	first := postTo(t, w.ts, "/v1/payment-orders", key, paymentBody(a, 30000000, "simbreb", "@beneficiary"))
	again := postTo(t, w.ts, "/v1/payment-orders", key, paymentBody(a, 30000000, "simbreb", "@beneficiary"))

	if o := paid(t, first); o.Status != gen.PaymentOrderStatusHeld || o.Held == nil || o.Held.Amount != 30120000 || o.EndToEndId == "" || o.AccountId != a.Id {
		t.Fatalf("the order = %+v, want it held for 301,200 COP", o)
	}
	if again.status != http.StatusAccepted || !bytes.Equal(again.body, first.body) {
		t.Fatalf("the retry = %d %s, want the same 202", again.status, again.body)
	}
	if w.natural(t, a) != 100000000 {
		t.Fatalf("the balance = %d, want 1,000,000 COP until the payment settles", w.natural(t, a))
	}
}

// TestAPaymentScreeningDoesNotClearOverHTTP (AB-6, FR-42): a payment to a beneficiary that may match is 202
// pending_screening, holding nothing.
func TestAPaymentScreeningDoesNotClearOverHTTP(t *testing.T) {
	w, a := payingWorld(t)

	r := postTo(t, w.ts, "/v1/payment-orders", ids.New().String(), paymentBody(a, 30000000, "simbreb", "match-sanctioned"))

	if o := paid(t, r); o.Status != gen.PaymentOrderStatusPendingScreening || o.Held != nil {
		t.Fatalf("pay a possible match = %+v, want pending_screening without a hold", o)
	}
}

// TestAForgottenKeyDoesNotRepeatAPayment (FR-31): once the key store has forgotten a payment's Idempotency-Key, the
// same request with the same key is 409 duplicate_payment, not a second payment.
func TestAForgottenKeyDoesNotRepeatAPayment(t *testing.T) {
	w, a := payingWorld(t)
	key := ids.New().String()
	paid(t, postTo(t, w.ts, "/v1/payment-orders", key, paymentBody(a, 30000000, "simbreb", "@beneficiary")))
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `DELETE FROM infra.idempotency_keys WHERE key = $1`, key)
		return err
	})

	r := postTo(t, w.ts, "/v1/payment-orders", key, paymentBody(a, 30000000, "simbreb", "@beneficiary"))

	if r.status != http.StatusConflict || problemCode(t, r) != "duplicate_payment" {
		t.Fatalf("the same payment after the key was forgotten = %d %s, want 409 duplicate_payment", r.status, r.body)
	}
}

// TestARejectedPaymentLeavesNothingOverHTTP (FR-31, FR-42): a quoted payment without funds is 422 and leaves
// nothing: its quote unused, no screening case of the payment, no event, and no job of an order that is not.
func TestARejectedPaymentLeavesNothingOverHTTP(t *testing.T) {
	w, a := payingWorld(t)
	var q gen.Quote
	if status := call(t, w.ts, http.MethodPost, "/v1/quotes", quoteBody(a.Id.String(), "transfer", 99900000), &q); status != http.StatusCreated {
		t.Fatalf("quote = %d", status)
	}
	body := `{"account_id":"` + a.Id.String() + `","amount":{"amount":99900000,"currency":"COP"},"rail":"simbreb",` +
		`"creditor":{"key":"@beneficiary"},"quote_id":"` + q.Id.String() + `"}`

	r := postTo(t, w.ts, "/v1/payment-orders", ids.New().String(), body)

	var unused bool
	var cases, events, jobs int
	err := testPool.QueryRow(t.Context(), `SELECT (SELECT used_at IS NULL FROM pricing.quotes WHERE id = $1),
		(SELECT count(*) FROM compliance.cases WHERE legal_entity_id = $2 AND kind = 'payment'),
		(SELECT count(*) FROM infra.outbox WHERE legal_entity_id = $2 AND type LIKE 'aluna.payments.%'),
		(SELECT count(*) FROM infra.river_job WHERE kind = 'payments.send' AND args->>'order_id' NOT IN
			(SELECT id::text FROM payments.orders))`, q.Id, w.entity).Scan(&unused, &cases, &events, &jobs)
	if err != nil {
		t.Fatal(err)
	}
	if r.status != http.StatusUnprocessableEntity || !unused || cases != 0 || events != 0 || jobs != 0 {
		t.Fatalf("a payment without funds = %d, quote unused %v, %d cases, %d events, %d jobs; want 422 and nothing left",
			r.status, unused, cases, events, jobs)
	}
}

// TestAWrongPaymentIsAProblem (FR-31): a payment without funds is 422 insufficient_funds, through an unknown
// rail 422 invalid_payment_order, and from an account of nowhere 404.
func TestAWrongPaymentIsAProblem(t *testing.T) {
	w, a := payingWorld(t)
	nowhere := gen.DepositAccount{Id: [16]byte(ids.New())}
	cases := map[string]struct {
		body   string
		status int
		code   string
	}{
		"no funds":              {paymentBody(a, 99900000, "simbreb", "@beneficiary"), http.StatusUnprocessableEntity, "insufficient_funds"},
		"an unknown rail":       {paymentBody(a, 30000000, "swift", "@beneficiary"), http.StatusUnprocessableEntity, "invalid_payment_order"},
		"an account of nowhere": {paymentBody(nowhere, 30000000, "simbreb", "@beneficiary"), http.StatusNotFound, "not_found"},
	}
	for name, tc := range cases {
		r := postTo(t, w.ts, "/v1/payment-orders", ids.New().String(), tc.body)
		if r.status != tc.status || problemCode(t, r) != tc.code {
			t.Errorf("%s = %d %s, want %d %s", name, r.status, r.body, tc.status, tc.code)
		}
	}
}
