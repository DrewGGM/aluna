package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/deposits/internal/domain"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
)

// ErrStatementNotFound means an account without a statement of that month: its period is not closed yet or the
// statements job has not reached it, the account did not exist then, or it was closed and did not move in it.
var ErrStatementNotFound = errors.New("the account has no statement of that month")

// What a movement of a statement is (FR-45).
const (
	kindMovement = "movement"
	kindInterest = "interest"
	kindFee      = "fee"
	kindGMF      = "gmf"
)

// StatementEntry is an entry of an account's ledger account in a month, with its transaction and its counter: the
// other entry of its pair, since every posting rule writes an entry and its counter one after the other (Counter is
// the zero ID if it has none). Units have the ledger's sign: a debit is positive.
type StatementEntry struct {
	TransactionID           ids.ID
	Code                    string
	BusinessDate, ValueDate calendar.Date
	PostedAt                time.Time
	Units                   int64
	Counter                 ids.ID
	CounterUnits            int64
}

// Month is what the ledger has of an account in a month, by business date: what its entries before it add up to
// (Opening, with the ledger's sign) and its entries in (posted_at, id) order.
type Month struct {
	Opening int64
	Entries []StatementEntry
}

// CounterAccounts are the ledger accounts an account's interest comes from and its fees and GMF go to (FR-26, FR-28,
// FR-29); the zero ID if its product names one that does not exist.
type CounterAccounts struct{ InterestExpense, FeeIncome, GMFPayable ids.ID }

// Statement is an account's statement of a month (FR-45): its document, canonical JSON, the SHA-256 of those bytes,
// in hex, and once stored, when it was generated.
type Statement struct {
	Document    []byte
	ContentHash string
	GeneratedAt time.Time
}

// statementDocument is the JSON of a statement. Its fields go in this order and it carries nothing of when it was
// generated, so the same month always gives the same bytes. Amounts are in minor units, from the customer's side:
// what comes in is positive.
type statementDocument struct {
	AccountID      ids.ID          `json:"account_id"`
	Period         string          `json:"period"`
	Currency       string          `json:"currency"`
	OpeningBalance int64           `json:"opening_balance"`
	Interest       int64           `json:"interest"`
	Fees           int64           `json:"fees"`
	GMF            int64           `json:"gmf"`
	ClosingBalance int64           `json:"closing_balance"`
	Movements      []statementLine `json:"movements"`
}

type statementLine struct {
	TransactionID ids.ID `json:"transaction_id"`
	Code          string `json:"code"`
	Kind          string `json:"kind"`
	BusinessDate  string `json:"business_date"`
	ValueDate     string `json:"value_date"`
	PostedAt      string `json:"posted_at"`
	Amount        int64  `json:"amount"`
}

// BuildStatement is account a's statement of period (yyyy-mm) from its month m (FR-45, US-16): its opening and closing
// balances, every entry as a movement, and apart the interest, the fees and the GMF. An entry is interest, a fee or
// the GMF when its counter is counters' interest expense, fee income or GMF payable account for as much: so a
// reversal, whose pairs are the original's with the opposite sign, takes them off again.
func BuildStatement(a domain.Account, period string, m Month, counters CounterAccounts) (Statement, error) {
	doc := statementDocument{AccountID: a.ID, Period: period, Currency: a.Currency.Code(), Movements: []statementLine{}}
	opening := money.New(customerSide(m.Opening), a.Currency)
	closing, interest, fees, gmf := opening, money.New(0, a.Currency), money.New(0, a.Currency), money.New(0, a.Currency)
	for _, e := range m.Entries {
		line := lineOf(e, counters)
		amount := money.New(line.Amount, a.Currency)
		var err error
		if closing, err = closing.Add(amount); err != nil {
			return Statement{}, fmt.Errorf("statement of %s: %w", a.ID, err)
		}
		switch line.Kind {
		case kindInterest:
			interest, err = interest.Add(amount)
		case kindFee:
			fees, err = fees.Sub(amount)
		case kindGMF:
			gmf, err = gmf.Sub(amount)
		}
		if err != nil {
			return Statement{}, fmt.Errorf("statement of %s: %w", a.ID, err)
		}
		doc.Movements = append(doc.Movements, line)
	}
	doc.OpeningBalance, doc.ClosingBalance = opening.Units(), closing.Units()
	doc.Interest, doc.Fees, doc.GMF = interest.Units(), fees.Units(), gmf.Units()
	document, err := json.Marshal(doc)
	if err != nil {
		return Statement{}, fmt.Errorf("statement of %s: %w", a.ID, err)
	}
	sum := sha256.Sum256(document)
	return Statement{Document: document, ContentHash: hex.EncodeToString(sum[:])}, nil
}

// customerSide is units of the account, a liability, as its customer sees them: a credit is money in.
func customerSide(units int64) int64 { return ledger.NaturalBalance(ledger.KindLiability, units) }

// lineOf is entry e as a statement line, of the kind its counter says.
func lineOf(e StatementEntry, counters CounterAccounts) statementLine {
	line := statementLine{
		TransactionID: e.TransactionID, Code: e.Code, Kind: kindMovement, Amount: customerSide(e.Units),
		BusinessDate: e.BusinessDate.String(), ValueDate: e.ValueDate.String(), PostedAt: e.PostedAt.UTC().Format(time.RFC3339Nano),
	}
	if e.CounterUnits != -e.Units {
		return line
	}
	switch e.Counter {
	case counters.InterestExpense:
		line.Kind = kindInterest
	case counters.FeeIncome:
		line.Kind = kindFee
	case counters.GMFPayable:
		line.Kind = kindGMF
	}
	return line
}

// Statements is what generating statements needs, within the transaction a batch runs in.
type Statements interface {
	// Pending are up to batch accounts of the entity, in id order after after, that period (from to to, excluded) has
	// to state and has not: opened before to, and not closed, or closed but moved in it.
	Pending(ctx context.Context, entity ids.ID, period string, from, to calendar.Date, after ids.ID, batch int) ([]domain.Account, error)
	CounterAccounts(ctx context.Context, a domain.Account) (CounterAccounts, error)
	// Month reads a's month from from to to, excluded.
	Month(ctx context.Context, a domain.Account, from, to calendar.Date) (Month, error)
	InsertStatement(ctx context.Context, a domain.Account, period string, st Statement) error
	// CompletePeriod records that every account the entity's period has to state has its statement.
	CompletePeriod(ctx context.Context, entity ids.ID, period string) error
}

// StatementBatch generates a batch of the entity's statements of period, a closed one (FR-45), and returns how many
// it generated (fewer than batch once none is left, and then the period is complete) and the last account's id,
// where the next batch starts.
func StatementBatch(ctx context.Context, s Statements, entity ids.ID, period string, after ids.ID, batch int) (int, ids.ID, error) {
	start, err := time.Parse("2006-01", period)
	if err != nil {
		return 0, after, fmt.Errorf("statement period %q: %w", period, err)
	}
	from, to := calendar.DateOf(start), calendar.DateOf(start.AddDate(0, 1, 0))
	accounts, err := s.Pending(ctx, entity, period, from, to, after, batch)
	if err != nil {
		return 0, after, err
	}
	for _, a := range accounts {
		if err := generateStatement(ctx, s, a, period, from, to); err != nil {
			return 0, after, err
		}
	}
	if len(accounts) < batch {
		return len(accounts), after, s.CompletePeriod(ctx, entity, period)
	}
	return len(accounts), accounts[len(accounts)-1].ID, nil
}

// generateStatement generates and stores a's statement of period.
func generateStatement(ctx context.Context, s Statements, a domain.Account, period string, from, to calendar.Date) error {
	counters, err := s.CounterAccounts(ctx, a)
	if err != nil {
		return err
	}
	m, err := s.Month(ctx, a, from, to)
	if err != nil {
		return err
	}
	st, err := BuildStatement(a, period, m, counters)
	if err != nil {
		return err
	}
	return s.InsertStatement(ctx, a, period, st)
}
