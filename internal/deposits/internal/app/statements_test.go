package app_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/deposits/internal/app"
	"github.com/DrewGGM/aluna/internal/deposits/internal/domain"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
)

// statementLine is a movement of a statement as these tests read it.
type statementLine struct {
	Code   string `json:"code"`
	Kind   string `json:"kind"`
	Amount int64  `json:"amount"`
}

// statementDocument is a statement's document as these tests read it.
type statementDocument struct {
	AccountID      string          `json:"account_id"`
	Period         string          `json:"period"`
	Currency       string          `json:"currency"`
	OpeningBalance int64           `json:"opening_balance"`
	Interest       int64           `json:"interest"`
	Fees           int64           `json:"fees"`
	GMF            int64           `json:"gmf"`
	ClosingBalance int64           `json:"closing_balance"`
	Movements      []statementLine `json:"movements"`
}

// month builds the entries of a month of an account: each one moves units on it (the ledger's sign) against its
// counter, which moves the opposite.
type month struct {
	counters app.CounterAccounts
	other    ids.ID // the counter of the movements that are not charges nor interest
	entries  []app.StatementEntry
}

func newMonth() *month {
	return &month{counters: app.CounterAccounts{InterestExpense: ids.New(), FeeIncome: ids.New(), GMFPayable: ids.New()}, other: ids.New()}
}

func (m *month) add(tx ids.ID, code string, day int, units int64, counter ids.ID) *month {
	on := calendar.Date{Year: 2026, Month: time.February, Day: day}
	m.entries = append(m.entries, app.StatementEntry{
		TransactionID: tx, Code: code, BusinessDate: on, ValueDate: on, PostedAt: time.Date(2026, time.February, day, 15, 4, 5, 123456000, time.UTC),
		Units: units, Counter: counter, CounterUnits: -units,
	})
	return m
}

func account(t *testing.T) domain.Account {
	t.Helper()
	cop, err := money.CurrencyOf("COP")
	if err != nil {
		t.Fatal(err)
	}
	return domain.Account{ID: ids.New(), LedgerAccountID: ids.New(), Currency: cop}
}

func build(t *testing.T, a domain.Account, opening int64, m *month) (app.Statement, statementDocument) {
	t.Helper()
	st, err := app.BuildStatement(a, "2026-02", app.Month{Opening: opening, Entries: m.entries}, m.counters)
	if err != nil {
		t.Fatal(err)
	}
	var doc statementDocument
	if err := json.Unmarshal(st.Document, &doc); err != nil {
		t.Fatal(err)
	}
	return st, doc
}

// february is a month of an account that holds 1,000 when it starts (the account is a liability: money in is a
// credit, negative): a deposit; a withdrawal of 5 that pays a fee of 5 and the GMF; money received by transfer for
// as much as the fee its sender paid; and the interest capitalized.
func february() *month {
	m := newMonth()
	withdrawal, transfer := ids.New(), ids.New()
	return m.add(ids.New(), "deposits.cash_deposit", 2, -500, m.other).
		add(withdrawal, "deposits.withdrawal", 3, 5, m.other).
		add(withdrawal, "deposits.withdrawal", 3, 5, m.counters.FeeIncome).
		add(withdrawal, "deposits.withdrawal", 3, 3, m.counters.GMFPayable).
		add(transfer, "deposits.internal_transfer", 5, -5, m.other).
		add(ids.New(), "deposits.interest_capitalization", 28, -7, m.counters.InterestExpense)
}

// TestAStatementShowsTheMonthByKind (FR-45, US-16): its balances are the customer's, an entry is a fee, the GMF or
// interest by the account its counter is on, and the rest are movements, also one as large as a fee.
func TestAStatementShowsTheMonthByKind(t *testing.T) {
	a := account(t)

	_, got := build(t, a, -1000, february())

	want := statementDocument{
		AccountID: a.ID.String(), Period: "2026-02", Currency: "COP",
		OpeningBalance: 1000, Interest: 7, Fees: 5, GMF: 3, ClosingBalance: 1000 + 500 - 5 - 5 - 3 + 5 + 7,
		Movements: []statementLine{
			{"deposits.cash_deposit", "movement", 500},
			{"deposits.withdrawal", "movement", -5},
			{"deposits.withdrawal", "fee", -5},
			{"deposits.withdrawal", "gmf", -3},
			{"deposits.internal_transfer", "movement", 5},
			{"deposits.interest_capitalization", "interest", 7},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("statement\n got %+v\nwant %+v", got, want)
	}
}

// TestAStatementIsCanonical: built again from the same month it is the same bytes, and its hash is their SHA-256.
func TestAStatementIsCanonical(t *testing.T) {
	a, m := account(t), february()
	first, _ := build(t, a, -1000, m)
	again, _ := build(t, a, -1000, m)
	sum := sha256.Sum256(first.Document)
	if string(first.Document) != string(again.Document) || first.ContentHash != hex.EncodeToString(sum[:]) || again.ContentHash != first.ContentHash {
		t.Fatalf("statements %s (%s) and %s (%s)", first.Document, first.ContentHash, again.Document, again.ContentHash)
	}
	var line struct {
		Movements []struct {
			PostedAt     string `json:"posted_at"`
			BusinessDate string `json:"business_date"`
		} `json:"movements"`
	}
	if err := json.Unmarshal(first.Document, &line); err != nil {
		t.Fatal(err)
	}
	if m := line.Movements[0]; m.PostedAt != "2026-02-02T15:04:05.123456Z" || m.BusinessDate != "2026-02-02" {
		t.Errorf("first movement posted at %s on %s", m.PostedAt, m.BusinessDate)
	}
}

// TestAMonthWithoutMovementsKeepsItsBalance: an account that did not move still has its statement.
func TestAMonthWithoutMovementsKeepsItsBalance(t *testing.T) {
	_, got := build(t, account(t), -40, newMonth())
	if got.OpeningBalance != 40 || got.ClosingBalance != 40 || got.Movements == nil || len(got.Movements) != 0 {
		t.Errorf("statement %+v", got)
	}
}

// TestAReversalTakesTheChargesAndTheInterestOff: the pairs of a reversal are the original's with the opposite sign,
// so they count against the fee, the GMF and the interest they reverse.
func TestAReversalTakesTheChargesAndTheInterestOff(t *testing.T) {
	m := newMonth()
	m.add(ids.New(), "deposits.withdrawal", 3, 5, m.counters.FeeIncome).
		add(ids.New(), "deposits.withdrawal", 3, 3, m.counters.GMFPayable).
		add(ids.New(), "deposits.interest_capitalization", 28, -7, m.counters.InterestExpense)
	reversal := ids.New()
	m.add(reversal, "reversal", 28, -5, m.counters.FeeIncome).
		add(reversal, "reversal", 28, -3, m.counters.GMFPayable).
		add(reversal, "reversal", 28, 7, m.counters.InterestExpense)

	_, got := build(t, account(t), 0, m)

	if got.Fees != 0 || got.GMF != 0 || got.Interest != 0 || got.ClosingBalance != 0 {
		t.Errorf("statement %+v", got)
	}
}

// TestAnEntryIsAChargeOnlyForAsMuch: a counter on a charge account for another amount (an entry that is not its pair)
// is a movement.
func TestAnEntryIsAChargeOnlyForAsMuch(t *testing.T) {
	m := newMonth()
	m.add(ids.New(), "manual", 3, 5, m.counters.FeeIncome)
	m.entries[0].CounterUnits = -4

	_, got := build(t, account(t), 0, m)

	if got.Fees != 0 || got.Movements[0].Kind != "movement" {
		t.Errorf("statement %+v", got)
	}
}
