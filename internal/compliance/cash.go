package compliance

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	pg "github.com/DrewGGM/aluna/internal/compliance/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/compliance/internal/app"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// ChannelCash is the channel a cash movement posts with, the one the cash totals follow.
const ChannelCash = app.ChannelCash

// cashConsumer is the name of the consumer of the cash totals in the outbox's inbox.
const cashConsumer = "compliance.cash_totals"

// Holder is who holds a ledger account and the cash account its cash goes through.
type Holder = app.Holder

// HolderOf finds, within tx, who holds ledger account account of the entity; false if no party does.
type HolderOf func(ctx context.Context, tx postgres.Tx, entity, account ids.ID) (Holder, bool, error)

// postedEvent is what the cash totals read of aluna.ledger.transaction.posted.v1.
type postedEvent struct {
	Channel      string `json:"channel"`
	BusinessDate string `json:"business_date"`
	Entries      []struct {
		AccountID ids.ID `json:"account_id"`
		Amount    struct {
			Amount   int64  `json:"amount"`
			Currency string `json:"currency"`
		} `json:"amount"`
	} `json:"entries"`
}

// posted reads e as the posted transaction it tells.
func posted(e outbox.StoredEvent) (app.Posted, error) {
	var data postedEvent
	if err := json.Unmarshal(e.Data, &data); err != nil {
		return app.Posted{}, fmt.Errorf("read event %s: %w", e.ID, err)
	}
	businessDate, err := time.Parse(time.DateOnly, data.BusinessDate)
	if err != nil {
		return app.Posted{}, fmt.Errorf("event %s: business date: %w", e.ID, err)
	}
	t := app.Posted{LegalEntityID: e.LegalEntityID, Channel: data.Channel, BusinessDate: businessDate, Entries: make([]app.PostedEntry, len(data.Entries))}
	for i, entry := range data.Entries {
		c, err := money.CurrencyOf(entry.Amount.Currency)
		if err != nil {
			return app.Posted{}, fmt.Errorf("event %s: %w", e.ID, err)
		}
		t.Entries[i] = app.PostedEntry{AccountID: entry.AccountID, Amount: money.New(entry.Amount.Amount, c)}
	}
	return t, nil
}

// accumulation is app.CashAccumulation within tx.
type accumulation struct {
	pg.Store
	tx       postgres.Tx
	holderOf HolderOf
}

func (a accumulation) Holder(ctx context.Context, entity, account ids.ID) (Holder, bool, error) {
	return a.holderOf(ctx, a.tx, entity, account)
}

// FollowCash subscribes, at startup, the consumer of the cash totals to events.
func FollowCash(events *outbox.Outbox, holderOf HolderOf) {
	events.Subscribe(cashConsumer, []string{"aluna.ledger.transaction.posted.v1"}, AccumulatingCash(holderOf))
}

// AccumulatingCash is the consumer that keeps each party's cash totals (FR-43): holderOf says whose are the
// accounts of a transaction. The outbox's inbox delivers each event to it once.
func AccumulatingCash(holderOf HolderOf) outbox.Handler {
	return func(ctx context.Context, tx postgres.Tx, e outbox.StoredEvent) error {
		t, err := posted(e)
		if err != nil {
			return err
		}
		return app.AccumulateCash(ctx, accumulation{Store: pg.NewStore(tx), tx: tx, holderOf: holderOf}, t)
	}
}
