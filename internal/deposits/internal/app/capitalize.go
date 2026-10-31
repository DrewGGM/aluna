package app

import (
	"context"
	"fmt"
	"time"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/deposits/internal/domain/interest"
	"github.com/DrewGGM/aluna/internal/deposits/internal/domain/postingrules"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
)

// The capitalizations in the ledger: their source and transaction code, and who posts them (the accrual job,
// not a customer); each one's external reference is <account>:<period>, so a period is posted once.
const (
	capitalizationSource  = "deposits.capitalization"
	codeCapitalization    = "deposits.interest_capitalization"
	capitalizationActor   = "deposits.accrual"
	capitalizationChannel = "job"
)

// Capitalized is a capitalization posted: Amount of the interest of Period into AccountID, on ValueDate.
type Capitalized struct {
	AccountID, TransactionID ids.ID
	Period                   string
	ValueDate                calendar.Date
	Amount                   money.Amount
}

// Capitalizing is what capitalizing an account's interest needs, within the transaction of its accrual.
type Capitalizing interface {
	// AdjustDate moves d to the business day convention c dictates.
	AdjustDate(d calendar.Date, c calendar.Convention) calendar.Date
	// ExpenseAccount is the entity's ledger account with code, in currency c, or ErrProductAccountMissing.
	ExpenseAccount(ctx context.Context, entity ids.ID, code string, c money.Currency) (ids.ID, error)
	Post(ctx context.Context, in ledger.PostInput) (ledger.Transaction, error)
	EmitCapitalized(ctx context.Context, entity ids.ID, e Capitalized) error
}

// capitalization is a period of a product to capitalize on a day, against its interest expense account.
type capitalization struct {
	day         calendar.Date
	period      string
	expenseCode string
}

// capitalizeInterest posts into account a the interest it accrued and has not posted (interest.DueForPosting,
// rounded half even, ADR-0005), from its product's interest expense account, on value date cp.day, and returns
// the state with it posted (FR-26). Nothing due is not posted: the posting rules take no zero amount. It is
// not a customer movement: it checks no limits, counts nothing and is not customer activity.
func capitalizeInterest(ctx context.Context, c Capitalizing, entity ids.ID, cp capitalization, a DueAccount,
	state interest.AccrualState,
) (interest.AccrualState, error) {
	due, posted, err := interest.DueForPosting(state, a.Currency, money.HalfEven)
	if err != nil {
		return interest.AccrualState{}, err
	}
	if due == 0 {
		return state, nil
	}
	expense, err := c.ExpenseAccount(ctx, entity, cp.expenseCode, a.Currency)
	if err != nil {
		return interest.AccrualState{}, err
	}
	amount := money.New(due, a.Currency)
	entries, err := postingrules.InterestCapitalization(postingrules.Movement{Amount: amount, Account: a.LedgerAccountID, Counter: expense})
	if err != nil {
		return interest.AccrualState{}, err
	}
	tx, err := c.Post(ctx, ledger.PostInput{
		LegalEntityID: entity, Code: codeCapitalization, ValueDate: dateTime(cp.day), // the business date is the entity's today
		Source: capitalizationSource, ExternalRef: a.ID.String() + ":" + cp.period, Actor: capitalizationActor,
		Channel: capitalizationChannel, RuleVersion: postingrules.RuleVersion, Metadata: map[string]string{"rounding": "half_even"},
		Entries: entries,
	})
	if err != nil {
		return interest.AccrualState{}, err
	}
	if err := c.EmitCapitalized(ctx, entity, Capitalized{
		AccountID: a.ID, TransactionID: tx.ID, Period: cp.period, ValueDate: cp.day, Amount: amount,
	}); err != nil {
		return interest.AccrualState{}, err
	}
	return posted, nil
}

// frequency is a capitalization frequency of the products: how many months its period spans and how its
// period is named, from the year and the period's 0-based index within it.
type frequency struct {
	months int
	label  func(year, index int) string
}

var frequencies = map[string]frequency{
	"monthly":    {1, func(y, i int) string { return fmt.Sprintf("%d-%02d", y, i+1) }},
	"quarterly":  {3, func(y, i int) string { return fmt.Sprintf("%d-Q%d", y, i+1) }},
	"semiannual": {6, func(y, i int) string { return fmt.Sprintf("%d-H%d", y, i+1) }},
	"annual":     {12, func(y, _ int) string { return fmt.Sprintf("%d", y) }},
}

// capitalizationOn is the period whose capitalization date is day, if any (FR-26): a period is capitalized on
// its last day moved by convention c (adjust). A date moved forward lands in the next period, so day can
// capitalize the period before its own; one moved back stays in its own.
func capitalizationOn(day calendar.Date, name string, c calendar.Convention,
	adjust func(calendar.Date, calendar.Convention) calendar.Date,
) (string, bool, error) {
	f, ok := frequencies[name]
	if !ok {
		return "", false, fmt.Errorf("capitalization frequency %q", name)
	}
	own, ownEnd := f.periodOf(day)
	if adjust(ownEnd, c) == day {
		return own, true, nil
	}
	start := time.Date(ownEnd.Year, ownEnd.Month-time.Month(f.months)+1, 1, 0, 0, 0, 0, time.UTC)
	previous, previousEnd := f.periodOf(dateOfTime(start.AddDate(0, 0, -1)))
	if adjust(previousEnd, c) == day {
		return previous, true, nil
	}
	return "", false, nil
}

// periodOf is the label and the last day of the period day falls in: 2026-04 for a month, 2026-Q2 for a
// quarter, 2026-H1 for a half and 2026 for a year.
func (f frequency) periodOf(day calendar.Date) (string, calendar.Date) {
	index := (int(day.Month) - 1) / f.months
	end := dateOfTime(time.Date(day.Year, time.Month((index+1)*f.months+1), 0, 0, 0, 0, 0, time.UTC))
	return f.label(day.Year, index), end
}

func dateTime(d calendar.Date) time.Time {
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC)
}

func dateOfTime(t time.Time) calendar.Date {
	return calendar.Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}
}
