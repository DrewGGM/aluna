//go:build integration

package deposits_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/pricing"
	"github.com/DrewGGM/aluna/internal/products"
)

// withFees are the fees of a savings product: 2,500 COP a withdrawal and 1,000 COP a transfer.
var withFees = map[string]string{
	"fees": `[{"code": "withdrawal_fee", "trigger": "withdrawal", "amount_units": 250000},
		{"code": "transfer_fee", "trigger": "transfer", "amount_units": 100000}]`,
}

// accountOf is the entity's ledger account with code.
func (w world) accountOf(t *testing.T, code string) ledger.Account {
	t.Helper()
	a, err := w.ledger.AccountByCode(t.Context(), testPool, w.entity, code)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// withdrawn withdraws in, which must succeed, and returns the transaction.
func (w world) withdrawn(t *testing.T, in deposits.CashInput) ledger.Transaction {
	t.Helper()
	var posted ledger.Transaction
	inTx(t, func(tx postgres.Tx) error {
		var err error
		posted, err = w.deposits.Withdraw(t.Context(), tx, in)
		return err
	})
	return posted
}

// withdrawOf withdraws in.
func (w world) withdrawOf(t *testing.T, in deposits.CashInput) error {
	t.Helper()
	return try(t, func(tx postgres.Tx) error {
		_, err := w.deposits.Withdraw(t.Context(), tx, in)
		return err
	})
}

// uvt loads the UVT of 2026, 52,374 pesos.
func (w world) uvt(t *testing.T) {
	t.Helper()
	w.marketParameter(t, products.UVT, "52374")
}

// markExempt exempts account id from GMF.
func (w world) markExempt(t *testing.T, id ids.ID) error {
	t.Helper()
	return try(t, func(tx postgres.Tx) error {
		_, err := w.deposits.MarkGMFExempt(t.Context(), tx, w.entity, id)
		return err
	})
}

// quotedWithdrawal is the quote of a withdrawal of units from account id, which must be made.
func (w world) quotedWithdrawal(t *testing.T, id ids.ID, units int64) pricing.Quote {
	t.Helper()
	var q pricing.Quote
	inTx(t, func(tx postgres.Tx) error {
		in, err := w.deposits.QuoteInput(t.Context(), tx, w.entity, id, pricing.Withdrawal, money.New(units, copOf(t)))
		if err != nil {
			return err
		}
		q, err = w.pricing.Quote(t.Context(), tx, in)
		return err
	})
	return q
}

// charged are the entries of units moved out of account to counter, by the deposits rules.
func charged(t *testing.T, account, counter ids.ID, units int64) []ledger.EntryInput {
	t.Helper()
	return []ledger.EntryInput{{AccountID: account, Amount: money.New(units, copOf(t))}, {AccountID: counter, Amount: money.New(-units, copOf(t))}}
}

// TestAWithdrawalPostsItsFeeAndGMF (FR-28, FR-29): a withdrawal of 500,000 COP posts, in its transaction, its
// fee of 2,500 to the product's fee income account and its GMF of 2,000 to its GMF payable account, both
// async, and the account pays all three.
func TestAWithdrawalPostsItsFeeAndGMF(t *testing.T) {
	w := newWorld(t)
	vault := w.vault(t)
	a := w.openWith(t, withFees)
	w.deposited(t, a.ID, 100000000)

	posted := w.withdrawn(t, w.cashIn(50000000, a.ID, copOf(t)))

	income, payable := w.accountOf(t, "I.FEES.SAVINGS"), w.accountOf(t, "L.TAX.GMF")
	want := slices.Concat(charged(t, a.LedgerAccountID, vault, 50000000), charged(t, a.LedgerAccountID, income.ID, 250000),
		charged(t, a.LedgerAccountID, payable.ID, 200000))
	if !slices.Equal(posted.Entries, want) || w.balance(t, a) != 49550000 {
		t.Fatalf("withdrawal = %+v, balance %d; want entries %+v and 495,500 COP", posted.Entries, w.balance(t, a), want)
	}
	if income.Mode != ledger.BalanceModeAsync || payable.Mode != ledger.BalanceModeAsync {
		t.Fatalf("fee income %s and GMF payable %s, want both async", income.Mode, payable.Mode)
	}
}

// TestATransferChargesItsOrigin (FR-28, FR-29): the origin of a transfer of 30,000 COP pays its fee of 1,000
// and its GMF of 120; the destination receives the 30,000.
func TestATransferChargesItsOrigin(t *testing.T) {
	w := newWorld(t)
	w.vault(t)
	from, to := w.openWith(t, withFees), w.openWith(t, withFees)
	w.deposited(t, from.ID, 100000000)

	if err := w.transfer(t, from.ID, to.ID, 3000000); err != nil {
		t.Fatal(err)
	}

	if w.balance(t, from) != 96888000 || w.balance(t, to) != 3000000 {
		t.Fatalf("balances %d and %d, want 968,880 and 30,000 COP", w.balance(t, from), w.balance(t, to))
	}
}

// TestAQuotedWithdrawalIsChargedWhatItQuoted (FR-28, US-7): a withdrawal with a quote is charged exactly its
// fee and GMF; the same quote again, or a withdrawal's quote for a transfer, is ErrQuoteUnavailable and moves
// nothing.
func TestAQuotedWithdrawalIsChargedWhatItQuoted(t *testing.T) {
	w := newWorld(t)
	w.vault(t)
	a, b := w.openWith(t, withFees), w.openWith(t, withFees)
	w.deposited(t, a.ID, 100000000)
	q := w.quotedWithdrawal(t, a.ID, 50000000)
	in := w.cashIn(50000000, a.ID, copOf(t))
	in.QuoteID = q.ID

	w.withdrawn(t, in)
	in.ExternalRef = ids.New().String()
	again := w.withdrawOf(t, in)
	forTransfer := w.quotedWithdrawal(t, a.ID, 1000000)
	transferred := try(t, func(tx postgres.Tx) error {
		_, err := w.deposits.Transfer(t.Context(), tx, deposits.TransferInput{
			LegalEntityID: w.entity, From: a.ID, To: b.ID, QuoteID: forTransfer.ID, Amount: money.New(1000000, copOf(t)),
			ExternalRef: ids.New().String(), Actor: "teller-a", Channel: "branch",
		})
		return err
	})

	if q.GMF.Tax != money.New(200000, copOf(t)) || len(q.Fees) != 1 || w.balance(t, a) != 49550000 {
		t.Fatalf("quote %+v, balance %d; want the fee and 2,000 COP of GMF, charged: 495,500 COP", q, w.balance(t, a))
	}
	for name, err := range map[string]error{"the same quote again": again, "a withdrawal's quote for a transfer": transferred} {
		if !errors.Is(err, pricing.ErrQuoteUnavailable) {
			t.Errorf("%s = %v, want ErrQuoteUnavailable", name, err)
		}
	}
	if w.balance(t, a) != 49550000 || w.balance(t, b) != 0 {
		t.Fatalf("balances %d and %d, want them unchanged", w.balance(t, a), w.balance(t, b))
	}
}

// TestAnExemptAccountWithdrawsWithoutGMF (FR-29): an exempt account's withdrawals post no GMF while its
// party's exemption covers them, with a quote or without, and use it: after 1,000,000 COP, a quote of
// 18,000,000 pays on the 669,100 over the cap (2,676 COP). A closed account cannot be exempt, nor can an
// unknown one.
func TestAnExemptAccountWithdrawsWithoutGMF(t *testing.T) {
	w := newWorld(t)
	vault := w.vault(t)
	w.uvt(t)
	a, closed := w.active(t), w.active(t)
	inTx(t, func(tx postgres.Tx) error {
		return w.deposits.ChangeStatus(t.Context(), tx, w.entity, closed.ID, deposits.Closed, "teller-a")
	})
	if err := w.markExempt(t, a.ID); err != nil {
		t.Fatal(err)
	}
	w.deposited(t, a.ID, 100000000)

	posted := w.withdrawn(t, w.cashIn(50000000, a.ID, copOf(t)))
	quoted := w.cashIn(50000000, a.ID, copOf(t))
	quoted.QuoteID = w.quotedWithdrawal(t, a.ID, 50000000).ID
	w.withdrawn(t, quoted)
	rest := w.quotedWithdrawal(t, a.ID, 1800000000)

	if want := charged(t, a.LedgerAccountID, vault, 50000000); !slices.Equal(posted.Entries, want) || w.balance(t, a) != 0 {
		t.Fatalf("exempt withdrawal = %+v, balance %d; want only %+v and nothing left", posted.Entries, w.balance(t, a), want)
	}
	if rest.GMF.Tax != money.New(267600, copOf(t)) {
		t.Fatalf("GMF after 1,000,000 exempt = %+v, want 2,676 COP", rest.GMF)
	}
	if err := w.markExempt(t, closed.ID); !errors.Is(err, pricing.ErrGMFNotEligible) {
		t.Errorf("a closed account = %v, want ErrGMFNotEligible", err)
	}
	if err := w.markExempt(t, ids.New()); !errors.Is(err, deposits.ErrAccountNotFound) {
		t.Errorf("an unknown account = %v, want ErrAccountNotFound", err)
	}
}

// TestAChargeOfNothingNeedsNoAccount: an operation without fees and whose GMF rounds to nothing (4 per thousand
// of 100 COP) posts neither, so the product's fee income and GMF payable accounts need not exist.
func TestAChargeOfNothingNeedsNoAccount(t *testing.T) {
	w := newWorld(t)
	vault := w.vault(t)
	a := w.openWith(t, map[string]string{
		"accounts": `{"cash": "A.CASH.VAULT", "interest_expense": "E.INTEREST.SAVINGS", "fee_income": "I.FEES.NONE", "gmf_payable": "L.TAX.NONE"}`,
	})
	w.deposited(t, a.ID, 10000)

	posted := w.withdrawn(t, w.cashIn(10000, a.ID, copOf(t)))

	if want := charged(t, a.LedgerAccountID, vault, 10000); !slices.Equal(posted.Entries, want) {
		t.Fatalf("withdrawal = %+v, want only %+v", posted.Entries, want)
	}
}

// TestAnAccountDoesNotCloseWhileItIsExempted (FR-29): an exemption locks the account, as a close does: while a
// close is in flight it waits, and then finds the account closed (pricing.ErrGMFNotEligible).
func TestAnAccountDoesNotCloseWhileItIsExempted(t *testing.T) {
	w := newWorld(t)
	w.vault(t)
	a := w.active(t)
	done := make(chan error, 1)

	inTx(t, func(tx postgres.Tx) error {
		if err := w.deposits.ChangeStatus(t.Context(), tx, w.entity, a.ID, deposits.Closed, "teller-a"); err != nil {
			return err
		}
		go func() { done <- w.markExempt(t, a.ID) }()
		select {
		case err := <-done:
			return errors.Join(errors.New("the exemption did not wait for the close"), err)
		case <-time.After(300 * time.Millisecond):
			return nil
		}
	})

	if err := <-done; !errors.Is(err, pricing.ErrGMFNotEligible) {
		t.Fatalf("an exemption while the account closes = %v, want ErrGMFNotEligible", err)
	}
}
