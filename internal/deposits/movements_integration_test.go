//go:build integration

package deposits_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/party"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// vault creates, in COP, the internal accounts the products of the testdata post against: the cash account
// (A.CASH.VAULT), which cannot go below 0 and whose id it returns, and their interest expense accounts.
func (w world) vault(t *testing.T) ids.ID {
	t.Helper()
	return w.vaultIn(t, copOf(t))
}

// vaultIn is vault in currency c.
func (w world) vaultIn(t *testing.T, c money.Currency) ids.ID {
	t.Helper()
	var cash ledger.Account
	inTx(t, func(tx postgres.Tx) error {
		var err error
		if cash, err = w.ledger.CreateAccount(t.Context(), tx, ledger.CreateAccountInput{
			LegalEntityID: w.entity, Code: "A.CASH.VAULT", Kind: ledger.KindAsset, Currency: c, Mode: ledger.BalanceModeSync,
		}); err != nil {
			return err
		}
		for _, code := range []string{"E.INTEREST.SAVINGS", "E.INTEREST.SIMPLIFIED"} {
			if _, err := w.ledger.CreateAccount(t.Context(), tx, ledger.CreateAccountInput{
				LegalEntityID: w.entity, Code: code, Kind: ledger.KindExpense, Currency: c, Mode: ledger.BalanceModeSync,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	return cash.ID
}

// active opens an active savings account for a new party with KYC approved.
func (w world) active(t *testing.T) deposits.Account {
	t.Helper()
	savings, _ := w.product(t, "valid-savings.json", true)
	a, err := w.open(t, w.party(t, party.KYCApproved), savings.ID)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// openWith opens an active account of the savings product with the given fields of its definition replaced
// (each a JSON value).
func (w world) openWith(t *testing.T, fields map[string]string) deposits.Account {
	t.Helper()
	var definition map[string]json.RawMessage
	if err := json.Unmarshal(fixture(t, "valid-savings.json"), &definition); err != nil {
		t.Fatal(err)
	}
	for name, value := range fields {
		definition[name] = json.RawMessage(value)
	}
	document, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := w.productOf(t, document, true)
	a, err := w.open(t, w.party(t, party.KYCApproved), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// cashIn is a movement of units of c on account id, with a new reference.
func (w world) cashIn(units int64, id ids.ID, c money.Currency) deposits.CashInput {
	return deposits.CashInput{
		LegalEntityID: w.entity, AccountID: id, Amount: money.New(units, c), ExternalRef: ids.New().String(), Actor: "teller-a", Channel: "branch",
	}
}

func (w world) deposit(t *testing.T, id ids.ID, units int64) error {
	t.Helper()
	return w.depositOf(t, w.cashIn(units, id, copOf(t)))
}

func (w world) depositOf(t *testing.T, in deposits.CashInput) error {
	t.Helper()
	return try(t, func(tx postgres.Tx) error {
		_, err := w.deposits.Deposit(t.Context(), tx, in)
		return err
	})
}

// deposited deposits units into account id, which must succeed, and returns the transaction.
func (w world) deposited(t *testing.T, id ids.ID, units int64) ledger.Transaction {
	t.Helper()
	var posted ledger.Transaction
	inTx(t, func(tx postgres.Tx) error {
		var err error
		posted, err = w.deposits.Deposit(t.Context(), tx, w.cashIn(units, id, copOf(t)))
		return err
	})
	return posted
}

func (w world) withdraw(t *testing.T, id ids.ID, units int64) error {
	t.Helper()
	return try(t, func(tx postgres.Tx) error {
		_, err := w.deposits.Withdraw(t.Context(), tx, w.cashIn(units, id, copOf(t)))
		return err
	})
}

func (w world) transfer(t *testing.T, from, to ids.ID, units int64) error {
	t.Helper()
	return try(t, func(tx postgres.Tx) error {
		_, err := w.deposits.Transfer(t.Context(), tx, deposits.TransferInput{
			LegalEntityID: w.entity, From: from, To: to, Amount: money.New(units, copOf(t)), ExternalRef: ids.New().String(),
			Actor: "teller-a", Channel: "branch",
		})
		return err
	})
}

// balance is the natural balance of a deposit account: what the customer has.
func (w world) balance(t *testing.T, a deposits.Account) int64 {
	t.Helper()
	return -w.posted(t, a.LedgerAccountID)
}

func (w world) posted(t *testing.T, ledgerAccountID ids.ID) int64 {
	t.Helper()
	b, err := w.ledger.Balance(t.Context(), testPool, ledgerAccountID)
	if err != nil {
		t.Fatal(err)
	}
	return b.Posted
}

// TestMoneyMovesBetweenAccounts (US-5, FR-23): open two accounts, deposit 100,000 into the first and transfer
// 30,000 to the second: balances 70,000 and 30,000, and the vault holds the 100,000; 10,000 back from the
// second (the account with the higher id, which a transfer locks second) leaves 80,000 and 20,000. The
// deposit is a transaction of the deposit rules on the business date, with its actor and channel.
func TestMoneyMovesBetweenAccounts(t *testing.T) {
	w := newWorld(t)
	vault := w.vault(t)
	first, second := w.active(t), w.active(t)

	deposited := w.deposited(t, first.ID, 10000000)
	if err := w.transfer(t, first.ID, second.ID, 3000000); err != nil {
		t.Fatal(err)
	}

	if w.balance(t, first) != 7000000 || w.balance(t, second) != 3000000 || w.posted(t, vault) != 10000000 {
		t.Fatalf("balances = %d and %d, vault %d; want 7000000, 3000000 and 10000000",
			w.balance(t, first), w.balance(t, second), w.posted(t, vault))
	}
	expectCashDeposit(t, deposited, vault, first.LedgerAccountID, 10000000)
	if err := w.transfer(t, second.ID, first.ID, 1000000); err != nil || w.balance(t, first) != 8000000 || w.balance(t, second) != 2000000 {
		t.Fatalf("transfer back = %v; balances %d and %d, want 8000000 and 2000000", err, w.balance(t, first), w.balance(t, second))
	}
}

// expectCashDeposit checks that posted is a cash deposit of units from vault into account, by the deposit
// rules, on 2026-03-02, with the world's actor and channel.
func expectCashDeposit(t *testing.T, posted ledger.Transaction, vault, account ids.ID, units int64) {
	t.Helper()
	today := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	want := []ledger.EntryInput{{AccountID: vault, Amount: money.New(units, copOf(t))}, {AccountID: account, Amount: money.New(-units, copOf(t))}}
	header := posted.Code == "deposits.cash_deposit" && posted.RuleVersion == "deposits/v1" && posted.Source == "deposits" &&
		posted.Actor == "teller-a" && posted.Channel == "branch"
	if !header || !posted.ValueDate.Equal(today) || !posted.BusinessDate.Equal(today) || !slices.Equal(posted.Entries, want) {
		t.Fatalf("deposit = %+v, want the cash deposit rule on 2026-03-02 with entries %+v", posted, want)
	}
}

// TestAWithdrawalNeedsFunds: a withdrawal of more than the balance is rejected and posts nothing; one of all
// of it empties the account and returns the cash to the vault.
func TestAWithdrawalNeedsFunds(t *testing.T) {
	w := newWorld(t)
	vault := w.vault(t)
	a := w.active(t)
	w.deposited(t, a.ID, 1000)

	tooMuch := w.withdraw(t, a.ID, 1001)
	all := w.withdraw(t, a.ID, 1000)

	if !errors.Is(tooMuch, ledger.ErrInsufficientFunds) || all != nil || w.balance(t, a) != 0 || w.posted(t, vault) != 0 {
		t.Fatalf("withdraw 1001 = %v, then 1000 = %v; balance %d, vault %d; want ErrInsufficientFunds, then nil and 0",
			tooMuch, all, w.balance(t, a), w.posted(t, vault))
	}
}

// TestOnlyAnActiveAccountMovesMoney (FR-22): a pending, frozen, dormant or closed account takes no deposit,
// withdrawal or transfer, in or out, even with funds; nothing is posted.
func TestOnlyAnActiveAccountMovesMoney(t *testing.T) {
	w := newWorld(t)
	w.vault(t)
	funded := w.active(t)
	w.deposited(t, funded.ID, 1000)
	for _, status := range []deposits.Status{deposits.Pending, deposits.Frozen, deposits.Dormant, deposits.Closed} {
		t.Run(string(status), func(t *testing.T) {
			a := w.inStatus(t, status)
			before := w.balance(t, a)
			moves := map[string]error{
				"deposit":      w.deposit(t, a.ID, 100),
				"withdrawal":   w.withdraw(t, a.ID, 100),
				"transfer in":  w.transfer(t, funded.ID, a.ID, 100),
				"transfer out": w.transfer(t, a.ID, funded.ID, 100),
			}
			for move, err := range moves {
				if !errors.Is(err, deposits.ErrAccountNotActive) {
					t.Errorf("%s = %v, want ErrAccountNotActive", move, err)
				}
			}
			if w.balance(t, a) != before || w.balance(t, funded) != 1000 {
				t.Fatalf("balances = %d and %d, want %d and 1000: nothing posted", w.balance(t, a), w.balance(t, funded), before)
			}
		})
	}
}

// inStatus is an account in status; a frozen or dormant one has 500 deposited while it was active.
func (w world) inStatus(t *testing.T, status deposits.Status) deposits.Account {
	t.Helper()
	if status == deposits.Pending {
		a, _ := w.pendingAccount(t)
		return a
	}
	a := w.active(t)
	if status != deposits.Closed {
		w.deposited(t, a.ID, 500)
	}
	if err := w.change(t, a.ID, status); err != nil {
		t.Fatal(err)
	}
	return a
}

// TestAMovementIsChecked: an amount that is not positive, a transfer to the same account, another currency,
// another entity's account and a product whose cash account does not exist, or is in another currency, are
// rejected.
func TestAMovementIsChecked(t *testing.T) {
	w := newWorld(t)
	w.vault(t)
	a := w.active(t)
	usd, err := money.CurrencyOf("USD")
	if err != nil {
		t.Fatal(err)
	}
	other := newWorld(t) // without a vault
	theirs := other.active(t)
	dollars := newWorld(t) // with its vault in USD
	dollars.vaultIn(t, usd)
	inPesos := dollars.active(t)

	for name, c := range map[string]struct{ got, want error }{
		"zero amount":                {w.deposit(t, a.ID, 0), deposits.ErrInvalidMovement},
		"negative amount":            {w.deposit(t, a.ID, -100), deposits.ErrInvalidMovement},
		"transfer to itself":         {w.transfer(t, a.ID, a.ID, 100), deposits.ErrInvalidMovement},
		"in dollars":                 {w.depositOf(t, w.cashIn(100, a.ID, usd)), ledger.ErrCurrencyMismatch},
		"another entity's account":   {w.deposit(t, theirs.ID, 100), deposits.ErrAccountNotFound},
		"a product without its cash": {other.deposit(t, theirs.ID, 100), deposits.ErrProductAccountMissing},
		"a cash in another currency": {dollars.deposit(t, inPesos.ID, 100), deposits.ErrProductAccountMissing},
	} {
		if !errors.Is(c.got, c.want) {
			t.Errorf("%s = %v, want %v", name, c.got, c.want)
		}
	}
	if w.balance(t, a) != 0 || other.balance(t, theirs) != 0 || dollars.balance(t, inPesos) != 0 {
		t.Fatal("a rejected movement posted")
	}
}

// TestAReferenceMovesMoneyOnce: a second movement with the reference of one already posted is
// ledger.ErrDuplicateExternalRef and posts nothing.
func TestAReferenceMovesMoneyOnce(t *testing.T) {
	w := newWorld(t)
	w.vault(t)
	a := w.active(t)
	in := w.cashIn(1000, a.ID, copOf(t))
	if err := w.depositOf(t, in); err != nil {
		t.Fatal(err)
	}

	again := w.depositOf(t, in)

	if !errors.Is(again, ledger.ErrDuplicateExternalRef) || w.balance(t, a) != 1000 {
		t.Fatalf("the same reference again = %v with a balance of %d, want ErrDuplicateExternalRef and 1000", again, w.balance(t, a))
	}
}

// TestTheLastCustomerActivityIsTheBusinessDate: a movement records the business date as the last customer
// activity of the account it is done on; the destination of a transfer keeps its own.
func TestTheLastCustomerActivityIsTheBusinessDate(t *testing.T) {
	w := newWorld(t)
	w.vault(t)
	a, b := w.active(t), w.active(t)
	monday, tuesday := calendar.Date{Year: 2026, Month: time.March, Day: 2}, calendar.Date{Year: 2026, Month: time.March, Day: 3}
	inTx(t, func(tx postgres.Tx) error {
		_, err := calendar.New().Rollover(t.Context(), tx, w.entity, monday)
		return err
	})

	deposited := w.deposited(t, a.ID, 1000)
	if err := w.transfer(t, a.ID, b.ID, 400); err != nil {
		t.Fatal(err)
	}

	on := deposited.BusinessDate
	if (calendar.Date{Year: on.Year(), Month: on.Month(), Day: on.Day()}) != tuesday {
		t.Fatalf("deposit posted on %s, want %v", on, tuesday)
	}
	if got := w.reread(t, a).LastCustomerActivityOn; got != tuesday {
		t.Fatalf("last activity of the account that moved = %v, want %v", got, tuesday)
	}
	if got := w.reread(t, b).LastCustomerActivityOn; got != monday {
		t.Fatalf("last activity of the transfer's destination = %v, want its opening %v", got, monday)
	}
}

func (w world) reread(t *testing.T, a deposits.Account) deposits.Account {
	t.Helper()
	stored, err := w.deposits.Account(t.Context(), testPool, w.entity, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

// holdOpen runs fn in a transaction it keeps open; it returns the session's pid and finish, which commits
// it and returns its error.
func holdOpen(t *testing.T, fn func(tx postgres.Tx) error) (int32, func() error) {
	t.Helper()
	pids, done, release := make(chan int32, 1), make(chan error, 1), make(chan struct{})
	t.Cleanup(func() { close(release) }) // if the test fails earlier, the transaction is not left open
	go func() {
		done <- postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
			if err := fn(tx); err != nil {
				return err
			}
			var pid int32
			if err := tx.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return err
			}
			pids <- pid
			<-release
			return nil
		})
	}()
	select {
	case pid := <-pids:
		return pid, func() error { release <- struct{}{}; return <-done }
	case err := <-done:
		t.Fatalf("the open transaction failed: %v", err)
		return 0, nil
	}
}

// waitForBlockedBy waits up to 10 s for some session to be blocked by session pid.
func waitForBlockedBy(t *testing.T, pid int32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var blocked int
		err := testPool.QueryRow(t.Context(),
			`SELECT count(*) FROM pg_locks WHERE NOT granted AND $1 = ANY(pg_blocking_pids(pid))`, pid).Scan(&blocked)
		if err != nil {
			t.Fatal(err)
		}
		if blocked > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the second transaction never waited for the first one's lock")
}

// race runs first in a transaction kept open, runs second while it is, waits for second to be blocked by
// first, commits first and returns what second got.
func race(t *testing.T, first, second func(tx postgres.Tx) error) error {
	t.Helper()
	pid, finish := holdOpen(t, first)
	results := make(chan error, 1)
	go func() { results <- try(t, second) }()
	waitForBlockedBy(t, pid)
	if err := finish(); err != nil {
		t.Fatalf("the first transaction = %v", err)
	}
	return <-results
}

// TestACloseAndAMovementDoNotOverlap (FR-22): a close that comes during a deposit waits for it and finds the
// balance (ErrNotEmpty); a deposit that comes during a close waits for it and finds the account closed
// (ErrAccountNotActive). The account never ends up closed with a balance.
func TestACloseAndAMovementDoNotOverlap(t *testing.T) {
	w := newWorld(t)
	w.vault(t)
	deposit := func(a deposits.Account) func(tx postgres.Tx) error {
		return func(tx postgres.Tx) error {
			_, err := w.deposits.Deposit(t.Context(), tx, w.cashIn(100, a.ID, copOf(t)))
			return err
		}
	}
	closing := func(a deposits.Account) func(tx postgres.Tx) error {
		return func(tx postgres.Tx) error {
			return w.deposits.ChangeStatus(t.Context(), tx, w.entity, a.ID, deposits.Closed, "operator-a")
		}
	}

	depositFirst, closeFirst := w.active(t), w.active(t)
	lateClose := race(t, deposit(depositFirst), closing(depositFirst))
	lateDeposit := race(t, closing(closeFirst), deposit(closeFirst))

	if !errors.Is(lateClose, deposits.ErrNotEmpty) || w.reread(t, depositFirst).Status != deposits.Active || w.balance(t, depositFirst) != 100 {
		t.Fatalf("close during a deposit = %v, account %s with %d; want ErrNotEmpty and it active with 100",
			lateClose, w.reread(t, depositFirst).Status, w.balance(t, depositFirst))
	}
	if !errors.Is(lateDeposit, deposits.ErrAccountNotActive) || w.reread(t, closeFirst).Status != deposits.Closed || w.balance(t, closeFirst) != 0 {
		t.Fatalf("deposit during a close = %v, account %s with %d; want ErrAccountNotActive and it closed with 0",
			lateDeposit, w.reread(t, closeFirst).Status, w.balance(t, closeFirst))
	}
}
