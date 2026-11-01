// Package postgres stores payment orders in payments.orders.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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
// null, as is the hold of an order not held. A reference the entity already used is app.ErrDuplicatePayment,
// and it leaves the transaction usable.
func (s Store) InsertOrder(ctx context.Context, o app.Order) error {
	fees := make([]fee, len(o.Fees))
	for i, f := range o.Fees {
		fees[i] = fee{Code: f.Code, Units: f.Amount.Units()}
	}
	feesJSON, err := json.Marshal(fees)
	if err != nil {
		return fmt.Errorf("fees of order %s: %w", o.ID, err)
	}
	var holdID *ids.ID
	if o.HoldID != (ids.ID{}) {
		holdID = &o.HoldID
	}
	tag, err := s.q.Exec(ctx, `INSERT INTO payments.orders (id, legal_entity_id, account_id, direction, rail, end_to_end_id, instr_id,
		tx_id, currency, amount_units, creditor_number, creditor_key, status, reason, hold_id, held_units, fees, gmf_units, external_ref)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, nullif($11, ''), nullif($12, ''), $13, nullif($14, ''), $15, $16, $17, $18, $19)
		ON CONFLICT ON CONSTRAINT orders_external_ref_unique DO NOTHING`,
		o.ID, o.LegalEntityID, o.AccountID, o.Direction, o.Rail, o.EndToEndID, o.InstrID, o.TxID, o.Amount.Currency().Code(),
		o.Amount.Units(), o.Creditor.Number, o.Creditor.Key, o.Status, o.Reason, holdID, o.Held.Units(), feesJSON, o.GMF.Units(), o.ExternalRef)
	if err != nil {
		return fmt.Errorf("insert payment order %s: %w", o.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return app.ErrDuplicatePayment
	}
	return nil
}

// The queries that lock an order, by id and by end-to-end id: a beneficiary, a reason and a rail reference the
// order does not have are empty.
const (
	lockOrderSQL = `SELECT ` + orderColumns + ` FROM payments.orders WHERE id = $1 FOR UPDATE`
	lockByE2ESQL = `SELECT ` + orderColumns + ` FROM payments.orders WHERE end_to_end_id = $1 FOR UPDATE`
	orderColumns = `id, legal_entity_id, account_id, direction, rail, end_to_end_id, instr_id, tx_id, currency, amount_units,
		coalesce(creditor_number, ''), coalesce(creditor_key, ''), status, coalesce(reason, ''), coalesce(rail_ref, ''), external_ref,
		hold_id, held_units, fees, gmf_units`
)

// LockOrder locks (FOR UPDATE) and returns order id, or app.ErrOrderNotFound.
func (s Store) LockOrder(ctx context.Context, id ids.ID) (app.Order, error) {
	return s.lock(ctx, lockOrderSQL, id)
}

// LockByEndToEndID locks (FOR UPDATE) and returns the order with end-to-end id e2e, or app.ErrOrderNotFound.
func (s Store) LockByEndToEndID(ctx context.Context, e2e string) (app.Order, error) {
	return s.lock(ctx, lockByE2ESQL, e2e)
}

func (s Store) lock(ctx context.Context, query string, key any) (app.Order, error) {
	var (
		o                           app.Order
		direction, status, currency string
		amount, held, gmf           int64
		feesJSON                    []byte
		holdID                      *ids.ID // an order that holds nothing has none
	)
	err := s.q.QueryRow(ctx, query, key).Scan(&o.ID, &o.LegalEntityID, &o.AccountID, &direction, &o.Rail, &o.EndToEndID, &o.InstrID,
		&o.TxID, &currency, &amount, &o.Creditor.Number, &o.Creditor.Key, &status, &o.Reason, &o.RailRef, &o.ExternalRef, &holdID,
		&held, &feesJSON, &gmf)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Order{}, fmt.Errorf("%w: %v", app.ErrOrderNotFound, key)
	}
	if err != nil {
		return app.Order{}, fmt.Errorf("lock payment order %v: %w", key, err)
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
	o.Direction, o.Status = domain.Direction(direction), domain.Status(status)
	o.Amount, o.Held, o.GMF = money.New(amount, c), money.New(held, c), money.New(gmf, c)
	return o, nil
}

// UpdateOrder stores where o stands: its status, why it was rejected and the rail's reference.
func (s Store) UpdateOrder(ctx context.Context, o app.Order) error {
	if _, err := s.q.Exec(ctx, `UPDATE payments.orders SET status = $2, reason = nullif($3, ''), rail_ref = nullif($4, '') WHERE id = $1`,
		o.ID, o.Status, o.Reason, o.RailRef); err != nil {
		return fmt.Errorf("update payment order %s: %w", o.ID, err)
	}
	return nil
}
