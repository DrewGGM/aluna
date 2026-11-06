package app

import (
	"context"
	"fmt"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
)

// ChannelCash is the channel of a ledger transaction that moves cash over a teller's counter.
const ChannelCash = "cash"

// CashDirection is which way cash went.
type CashDirection string

// Cash directions: into the entity's cash account, a deposit, or out of it, a withdrawal.
const (
	CashDeposit    CashDirection = "deposit"
	CashWithdrawal CashDirection = "withdrawal"
)

// PostedEntry is an entry of a posted transaction, debit positive.
type PostedEntry struct {
	AccountID ids.ID
	Amount    money.Amount
}

// Posted is a posted ledger transaction, as its event tells it.
type Posted struct {
	LegalEntityID ids.ID
	Channel       string
	BusinessDate  time.Time
	Entries       []PostedEntry
}

// Holder is who holds a ledger account and the cash account its cash goes through.
type Holder struct{ PartyID, CashAccount ids.ID }

// CashMovement is what a transaction adds to what a party moved in cash in a month, its first day.
type CashMovement struct {
	LegalEntityID, PartyID ids.ID
	Month                  time.Time
	Direction              CashDirection
	Amount                 money.Amount
}

// CashAccumulation is what accumulating cash needs.
type CashAccumulation interface {
	// Holder is who holds ledger account account of the entity; false if no party does.
	Holder(ctx context.Context, entity, account ids.ID) (Holder, bool, error)
	// AddCash adds m to its party's total of its month, direction and currency.
	AddCash(ctx context.Context, m CashMovement) error
}

// AccumulateCash adds a posted cash transaction to the total of the party holding one of its accounts, in the
// month of its business date (FR-43). What went through the cash account is what counts, not what the party's
// account moved, which includes the charges. A transaction of another channel, or one with no party (between
// the entity's own accounts), adds nothing.
func AccumulateCash(ctx context.Context, p CashAccumulation, t Posted) error {
	if t.Channel != ChannelCash {
		return nil
	}
	h, held, err := firstHolder(ctx, p, t)
	if err != nil || !held {
		return err
	}
	moved, err := throughCash(t, h.CashAccount)
	if err != nil || moved.IsZero() {
		return err
	}
	direction := CashDeposit
	if moved.Sign() < 0 {
		direction = CashWithdrawal
		if moved, err = moved.Neg(); err != nil {
			return fmt.Errorf("cash withdrawn: %w", err)
		}
	}
	year, month, _ := t.BusinessDate.Date()
	return p.AddCash(ctx, CashMovement{
		LegalEntityID: t.LegalEntityID, PartyID: h.PartyID, Month: time.Date(year, month, 1, 0, 0, 0, 0, time.UTC), Direction: direction, Amount: moved,
	})
}

// firstHolder is the holder of the first account of t that has one.
func firstHolder(ctx context.Context, p CashAccumulation, t Posted) (Holder, bool, error) {
	for _, e := range t.Entries {
		if h, held, err := p.Holder(ctx, t.LegalEntityID, e.AccountID); err != nil || held {
			return h, held, err
		}
	}
	return Holder{}, false, nil
}

// throughCash is what t moved through account cash, debit positive.
func throughCash(t Posted, cash ids.ID) (money.Amount, error) {
	var moved money.Amount
	found := false
	for _, e := range t.Entries {
		if e.AccountID != cash {
			continue
		}
		if !found {
			moved, found = e.Amount, true
			continue
		}
		sum, err := moved.Add(e.Amount)
		if err != nil {
			return money.Amount{}, fmt.Errorf("cash moved: %w", err)
		}
		moved = sum
	}
	return moved, nil
}
