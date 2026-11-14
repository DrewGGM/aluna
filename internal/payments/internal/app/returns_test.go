package app

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/payments/internal/domain"
	"github.com/DrewGGM/aluna/internal/pricing"
)

// TestEachOutcomeGoesItsOwnWay (FR-31): a return is no outcome of a sent order and only a return is taken as one,
// so neither rejects nor refunds by mistake; a return without its reason is invalid. None reaches the database.
func TestEachOutcomeGoesItsOwnWay(t *testing.T) {
	for name, err := range map[string]error{
		"a return as an outcome":    TakeOutcome(t.Context(), nil, RailOutcome{EndToEndID: "E2E", Status: RailReturned, Reason: "AC04"}, Policy{}),
		"a rejection as a return":   TakeReturn(t.Context(), nil, RailOutcome{EndToEndID: "E2E", Status: RailRejected, Reason: "AC04"}, Policy{}),
		"an acceptance as a return": TakeReturn(t.Context(), nil, RailOutcome{EndToEndID: "E2E", Status: RailAccepted}, Policy{}),
		"a return without a reason": TakeReturn(t.Context(), nil, RailOutcome{EndToEndID: "E2E", Status: RailReturned}, Policy{}),
	} {
		if !errors.Is(err, ErrInvalidNotification) {
			t.Errorf("%s = %v, want ErrInvalidNotification", name, err)
		}
	}
}

// fakeSettlement is a Returning and a Settling of one payment, recording what it posts and what it restores.
type fakeSettlement struct {
	order                                       Order
	customer, clearing, suspense, fees, payable ids.ID
	booked                                      []ledger.EntryInput
	restored                                    pricing.ExemptionUse
}

func (f *fakeSettlement) LockByEndToEndID(context.Context, string) (Order, error) {
	return f.order, nil
}

func (f *fakeSettlement) LockActive(context.Context, ids.ID, ids.ID) (deposits.Account, error) {
	return deposits.Account{LedgerAccountID: f.customer}, nil
}

func (f *fakeSettlement) RailAccounts(context.Context, ids.ID, string, money.Currency) (ids.ID, ids.ID, error) {
	return f.clearing, f.suspense, nil
}

func (f *fakeSettlement) Accounts(context.Context, Order) (ChargeAccounts, error) {
	return ChargeAccounts{Customer: f.customer, FeeIncome: f.fees, GMFPayable: f.payable}, nil
}

func (f *fakeSettlement) Book(_ context.Context, _ Order, _ string, entries []ledger.EntryInput, restore pricing.ExemptionUse) error {
	f.booked, f.restored = entries, restore
	return nil
}

func (f *fakeSettlement) Capture(context.Context, Order, []ledger.EntryInput) error { return nil }

func (f *fakeSettlement) Release(_ context.Context, _ Order, restore pricing.ExemptionUse) error {
	f.restored = restore
	return nil
}

func (f *fakeSettlement) Update(context.Context, Order) error { return nil }

// paymentOf is a fakeSettlement of a payment of 100,000 COP, in status, that charged a fee of 2,000 and a GMF of
// 400, and used 50,000 of its party's exemption.
func paymentOf(t *testing.T, status domain.Status) *fakeSettlement {
	t.Helper()
	cop, err := money.CurrencyOf("COP")
	if err != nil {
		t.Fatal(err)
	}
	o := Order{
		Order: domain.Order{ID: ids.New(), Direction: domain.Outgoing, Status: status, Amount: money.New(10000000, cop)},
		Fees:  []pricing.Fee{{Code: "transfer", Amount: money.New(200000, cop)}}, GMF: money.New(40000, cop),
		GMFExemption: pricing.ExemptionUse{Amount: money.New(5000000, cop), Month: time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)},
	}
	return &fakeSettlement{order: o, customer: ids.New(), clearing: ids.New(), suspense: ids.New(), fees: ids.New(), payable: ids.New()}
}

// credited is how much the entries credit account (a credit is negative in the ledger).
func credited(entries []ledger.EntryInput, account ids.ID) int64 {
	var units int64
	for _, e := range entries {
		if e.AccountID == account {
			units -= e.Amount.Units()
		}
	}
	return units
}

// TestAReturnRefundsTheChargesThePolicySays (FR-31, ADR-0016): a return gives the customer back the amount and, as
// the policy says, the fee; the GMF only if its reason annuls the payment, and what it used of the exemption only
// then and if the policy restores it.
func TestAReturnRefundsTheChargesThePolicySays(t *testing.T) {
	annulling := Policy{RefundFeesOnReturn: true, AnnullingReasons: []string{"AC01"}, RestoreExemption: true}
	keeping := annulling
	keeping.RestoreExemption = false
	cases := map[string]struct {
		policy                           Policy
		reason                           string
		customer, fees, payable, restore int64
	}{
		"annulled, refunding the fee":      {annulling, "AC01", 10240000, -200000, -40000, 5000000},
		"annulled, written otherwise":      {annulling, " ac01", 10240000, -200000, -40000, 5000000},
		"annulled, keeping the exemption":  {keeping, "AC01", 10240000, -200000, -40000, 0},
		"a voluntary return":               {annulling, "CUST", 10200000, -200000, 0, 0},
		"annulled, keeping the fee":        {Policy{AnnullingReasons: []string{"AC01"}}, "AC01", 10040000, 0, -40000, 0},
		"refunding nothing but the amount": {Policy{}, "AC01", 10000000, 0, 0, 0},
	}
	for name, tc := range cases {
		f := paymentOf(t, domain.StatusSettled)

		if err := TakeReturn(t.Context(), f, RailOutcome{EndToEndID: "E2E", Status: RailReturned, Reason: tc.reason}, tc.policy); err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		got := []int64{credited(f.booked, f.customer), credited(f.booked, f.fees), credited(f.booked, f.payable), f.restored.Amount.Units()}
		if want := []int64{tc.customer, tc.fees, tc.payable, tc.restore}; !slices.Equal(got, want) {
			t.Errorf("%s: customer, fee income, GMF payable credited and exemption restored = %v, want %v", name, got, want)
		}
	}
}

// TestARejectionRestoresTheExemptionThePolicySays (FR-31, ADR-0016): a payment the rail rejects never debited its
// GMF: what it used of the exemption is restored if the policy restores it, and kept otherwise.
func TestARejectionRestoresTheExemptionThePolicySays(t *testing.T) {
	for restore, want := range map[bool]int64{true: 5000000, false: 0} {
		f := paymentOf(t, domain.StatusSent)

		err := TakeOutcome(t.Context(), f, RailOutcome{EndToEndID: "E2E", Status: RailRejected, Reason: "AC04"}, Policy{RestoreExemption: restore})

		if err != nil || f.restored.Amount.Units() != want {
			t.Errorf("restoring %t: restored %v (%v), want %d", restore, f.restored, err, want)
		}
	}
}
