// Package postgres stores payment orders in payments.orders.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/payments/internal/app"
	"github.com/DrewGGM/aluna/internal/payments/internal/domain"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/pricing"
)

// Store is the orders over q.
type Store struct{ q postgres.Querier }

// NewStore is the store of payments over q, the pool or a Tx.
func NewStore(q postgres.Querier) Store { return Store{q: q} }

// fee is a fee as an order keeps it.
type fee struct {
	Code  string `json:"code"`
	Units int64  `json:"units"`
}

// InsertOrder stores o; the database sets created_at. A beneficiary goes by account or by key: the other is
// null, as are the hold of an order not held, the account of an incoming one for no account, its debtor agent
// if it is outgoing, and the rail's reference until the rail gives it. A reference the entity already used is
// app.ErrDuplicatePayment, and it leaves the transaction usable.
func (s Store) InsertOrder(ctx context.Context, o app.Order) error {
	feesJSON, err := feesOf(o)
	if err != nil {
		return err
	}
	tag, err := s.q.Exec(ctx, `INSERT INTO payments.orders (id, legal_entity_id, account_id, direction, rail, end_to_end_id, instr_id,
		tx_id, currency, amount_units, creditor_number, creditor_key, status, reason, hold_id, held_units, fees, gmf_units, external_ref,
		debtor_agent, rail_ref, debit_counted_on)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, nullif($11, ''), nullif($12, ''), $13, nullif($14, ''), $15, $16, $17, $18, $19,
		nullif($20, ''), nullif($21, ''), $22)
		ON CONFLICT ON CONSTRAINT orders_external_ref_unique DO NOTHING`,
		o.ID, o.LegalEntityID, orNull(o.AccountID), o.Direction, o.Rail, o.EndToEndID, o.InstrID, o.TxID, o.Amount.Currency().Code(),
		o.Amount.Units(), o.Creditor.Number, o.Creditor.Key, o.Status, o.Reason, orNull(o.HoldID), o.Held.Units(), feesJSON, o.GMF.Units(),
		o.ExternalRef, o.DebtorAgent, o.RailRef, dateOrNull(o.DebitCountedOn))
	if err != nil {
		return fmt.Errorf("insert payment order %s: %w", o.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return app.ErrDuplicatePayment
	}
	return nil
}

// feesOf is o's fees as payments.orders keeps them.
func feesOf(o app.Order) ([]byte, error) {
	fees := make([]fee, len(o.Fees))
	for i, f := range o.Fees {
		fees[i] = fee{Code: f.Code, Units: f.Amount.Units()}
	}
	b, err := json.Marshal(fees)
	if err != nil {
		return nil, fmt.Errorf("fees of order %s: %w", o.ID, err)
	}
	return b, nil
}

// dateOrNull is t, a business date at midnight, or null if it is zero.
func dateOrNull(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// orNull is id, or null if it is zero.
func orNull(id ids.ID) *ids.ID {
	if id == (ids.ID{}) {
		return nil
	}
	return &id
}

// The queries that lock an order, by id and an outgoing one by its end-to-end id, and the one that reads an outgoing
// order of an entity: a beneficiary, a reason, a rail reference and a debtor agent the order does not have are empty.
const (
	lockOrderSQL = `SELECT ` + orderColumns + ` FROM payments.orders WHERE id = $1 FOR UPDATE`
	lockByE2ESQL = `SELECT ` + orderColumns + ` FROM payments.orders WHERE direction = 'outgoing' AND end_to_end_id = $1 FOR UPDATE`
	outgoingSQL  = `SELECT ` + orderColumns + ` FROM payments.orders WHERE id = $1 AND legal_entity_id = $2 AND direction = 'outgoing'`
	orderColumns = `id, legal_entity_id, account_id, direction, rail, end_to_end_id, instr_id, tx_id, currency, amount_units,
		coalesce(creditor_number, ''), coalesce(creditor_key, ''), status, coalesce(reason, ''), coalesce(rail_ref, ''), external_ref,
		hold_id, held_units, fees, gmf_units, coalesce(debtor_agent, ''), debit_counted_on`
)

// LockOrder locks (FOR UPDATE) and returns order id, or app.ErrOrderNotFound.
func (s Store) LockOrder(ctx context.Context, id ids.ID) (app.Order, error) {
	return s.read(ctx, lockOrderSQL, id)
}

// LockByEndToEndID locks (FOR UPDATE) and returns the outgoing order with end-to-end id e2e, or
// app.ErrOrderNotFound.
func (s Store) LockByEndToEndID(ctx context.Context, e2e string) (app.Order, error) {
	return s.read(ctx, lockByE2ESQL, e2e)
}

// LockStalePending locks (FOR UPDATE SKIP LOCKED) and returns up to limit outgoing orders pending_screening made
// before before, the oldest first.
func (s Store) LockStalePending(ctx context.Context, before time.Time, limit int) ([]ids.ID, error) {
	rows, err := s.q.Query(ctx, `SELECT id FROM payments.orders WHERE direction = 'outgoing' AND status = 'pending_screening'
		AND created_at < $1 ORDER BY created_at LIMIT $2 FOR UPDATE SKIP LOCKED`, before, limit)
	if err != nil {
		return nil, fmt.Errorf("lock the stale pending orders: %w", err)
	}
	stale, err := pgx.CollectRows(rows, pgx.RowTo[ids.ID])
	if err != nil {
		return nil, fmt.Errorf("lock the stale pending orders: %w", err)
	}
	return stale, nil
}

// Outgoing reads outgoing order id of entity, or app.ErrOrderNotFound: another entity's order, or a transfer received,
// looks the same as a missing one.
func (s Store) Outgoing(ctx context.Context, entity, id ids.ID) (app.Order, error) {
	return s.read(ctx, outgoingSQL, id, entity)
}

// read returns the order query finds by key and the rest of args, locked if query locks it.
func (s Store) read(ctx context.Context, query string, key any, args ...any) (app.Order, error) {
	var (
		o                           app.Order
		direction, status, currency string
		amount, held, gmf           int64
		feesJSON                    []byte
		holdID, accountID           *ids.ID    // an order that holds nothing, or is for no account, has none
		counted                     *time.Time // nor has one that counted no debit
	)
	err := s.q.QueryRow(ctx, query, append([]any{key}, args...)...).Scan(&o.ID, &o.LegalEntityID, &accountID, &direction, &o.Rail,
		&o.EndToEndID, &o.InstrID, &o.TxID, &currency, &amount, &o.Creditor.Number, &o.Creditor.Key, &status, &o.Reason, &o.RailRef,
		&o.ExternalRef, &holdID, &held, &feesJSON, &gmf, &o.DebtorAgent, &counted)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Order{}, fmt.Errorf("%w: %v", app.ErrOrderNotFound, key)
	}
	if err != nil {
		return app.Order{}, fmt.Errorf("read payment order %v: %w", key, err)
	}
	c, err := money.CurrencyOf(currency)
	if err != nil {
		return app.Order{}, fmt.Errorf("payment order %s: %w", o.ID, err)
	}
	var fees []fee
	if err := json.Unmarshal(feesJSON, &fees); err != nil {
		return app.Order{}, fmt.Errorf("fees of order %s: %w", o.ID, err)
	}
	for _, f := range fees {
		o.Fees = append(o.Fees, pricing.Fee{Code: f.Code, Amount: money.New(f.Units, c)})
	}
	if holdID != nil {
		o.HoldID = *holdID
	}
	if accountID != nil {
		o.AccountID = *accountID
	}
	if counted != nil {
		o.DebitCountedOn = *counted
	}
	o.Direction, o.Status = domain.Direction(direction), domain.Status(status)
	o.Amount, o.Held, o.GMF = money.New(amount, c), money.New(held, c), money.New(gmf, c)
	return o, nil
}

// HoldOrder stores that o, which waited for compliance, is held: its status, its hold, the charges it covers and
// the day its debit was counted.
func (s Store) HoldOrder(ctx context.Context, o app.Order) error {
	feesJSON, err := feesOf(o)
	if err != nil {
		return err
	}
	if _, err := s.q.Exec(ctx, `UPDATE payments.orders SET status = $2, hold_id = $3, held_units = $4, fees = $5, gmf_units = $6,
		debit_counted_on = $7 WHERE id = $1`, o.ID, o.Status, o.HoldID, o.Held.Units(), feesJSON, o.GMF.Units(), dateOrNull(o.DebitCountedOn)); err != nil {
		return fmt.Errorf("hold payment order %s: %w", o.ID, err)
	}
	return nil
}

// UpdateOrder stores where o stands: its status, why it was rejected, the rail's reference and its account, which
// only a transfer credited from suspense changes (the database refuses any other change of it).
func (s Store) UpdateOrder(ctx context.Context, o app.Order) error {
	if _, err := s.q.Exec(ctx, `UPDATE payments.orders SET status = $2, reason = nullif($3, ''), rail_ref = nullif($4, ''), account_id = $5
		WHERE id = $1`, o.ID, o.Status, o.Reason, o.RailRef, orNull(o.AccountID)); err != nil {
		return fmt.Errorf("update payment order %s: %w", o.ID, err)
	}
	return nil
}
