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
	feesJSON, err := feesOf(o.ID, o.Fees)
	if err != nil {
		return err
	}
	var quotedJSON []byte // null: it was not quoted to wait for compliance
	if o.Quoted {
		if quotedJSON, err = feesOf(o.ID, o.QuotedFees); err != nil {
			return err
		}
	}
	tag, err := s.q.Exec(ctx, `INSERT INTO payments.orders (id, legal_entity_id, account_id, direction, rail, end_to_end_id, instr_id,
		tx_id, currency, amount_units, creditor_number, creditor_key, status, reason, hold_id, held_units, fees, gmf_units, external_ref,
		debtor_agent, rail_ref, debit_counted_on, gmf_exempt_units, gmf_exempt_month, quoted_fees)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, nullif($11, ''), nullif($12, ''), $13, nullif($14, ''), $15, $16, $17, $18, $19,
		nullif($20, ''), nullif($21, ''), $22, $23, $24, $25)
		ON CONFLICT ON CONSTRAINT orders_external_ref_unique DO NOTHING`,
		o.ID, o.LegalEntityID, orNull(o.AccountID), o.Direction, o.Rail, o.EndToEndID, o.InstrID, o.TxID, o.Amount.Currency().Code(),
		o.Amount.Units(), o.Creditor.Number, o.Creditor.Key, o.Status, o.Reason, orNull(o.HoldID), o.Held.Units(), feesJSON, o.GMF.Units(),
		o.ExternalRef, o.DebtorAgent, o.RailRef, dateOrNull(o.DebitCountedOn), o.GMFExemption.Amount.Units(), dateOrNull(o.GMFExemption.Month), quotedJSON)
	if err != nil {
		return fmt.Errorf("insert payment order %s: %w", o.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return app.ErrDuplicatePayment
	}
	return nil
}

// feesOf is fees, of order id, as payments.orders keeps them.
func feesOf(id ids.ID, fees []pricing.Fee) ([]byte, error) {
	kept := make([]fee, len(fees))
	for i, f := range fees {
		kept[i] = fee{Code: f.Code, Units: f.Amount.Units()}
	}
	b, err := json.Marshal(kept)
	if err != nil {
		return nil, fmt.Errorf("fees of order %s: %w", id, err)
	}
	return b, nil
}

// feesFrom are the fees document keeps, as feesOf writes them, in currency c.
func feesFrom(id ids.ID, document []byte, c money.Currency) ([]pricing.Fee, error) {
	var kept []fee
	if err := json.Unmarshal(document, &kept); err != nil {
		return nil, fmt.Errorf("fees of order %s: %w", id, err)
	}
	fees := make([]pricing.Fee, len(kept))
	for i, f := range kept {
		fees[i] = pricing.Fee{Code: f.Code, Amount: money.New(f.Units, c)}
	}
	return fees, nil
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
		hold_id, held_units, fees, gmf_units, coalesce(debtor_agent, ''), debit_counted_on, gmf_exempt_units, gmf_exempt_month, quoted_fees`
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
	var r orderRow
	err := s.q.QueryRow(ctx, query, append([]any{key}, args...)...).Scan(&r.o.ID, &r.o.LegalEntityID, &r.accountID, &r.direction,
		&r.o.Rail, &r.o.EndToEndID, &r.o.InstrID, &r.o.TxID, &r.currency, &r.amount, &r.o.Creditor.Number, &r.o.Creditor.Key, &r.status,
		&r.o.Reason, &r.o.RailRef, &r.o.ExternalRef, &r.holdID, &r.held, &r.feesJSON, &r.gmf, &r.o.DebtorAgent, &r.counted, &r.exempt,
		&r.exemptMonth, &r.quotedJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Order{}, fmt.Errorf("%w: %v", app.ErrOrderNotFound, key)
	}
	if err != nil {
		return app.Order{}, fmt.Errorf("read payment order %v: %w", key, err)
	}
	return r.order()
}

// orderRow is an order as payments.orders keeps it.
type orderRow struct {
	o                           app.Order
	direction, status, currency string
	amount, held, gmf, exempt   int64
	feesJSON, quotedJSON        []byte     // quoted_fees is null if it was not quoted to wait for compliance
	holdID, accountID           *ids.ID    // an order that holds nothing, or is for no account, has none
	counted, exemptMonth        *time.Time // nor has one that counted no debit, or used no GMF exemption
}

// order is the order r keeps.
func (r orderRow) order() (app.Order, error) {
	o := r.o
	c, err := money.CurrencyOf(r.currency)
	if err != nil {
		return app.Order{}, fmt.Errorf("payment order %s: %w", o.ID, err)
	}
	if o.Fees, err = feesFrom(o.ID, r.feesJSON, c); err != nil {
		return app.Order{}, err
	}
	if o.Quoted = r.quotedJSON != nil; o.Quoted {
		if o.QuotedFees, err = feesFrom(o.ID, r.quotedJSON, c); err != nil {
			return app.Order{}, err
		}
	}
	if r.holdID != nil {
		o.HoldID = *r.holdID
	}
	if r.accountID != nil {
		o.AccountID = *r.accountID
	}
	if r.counted != nil {
		o.DebitCountedOn = *r.counted
	}
	if r.exemptMonth != nil {
		o.GMFExemption = pricing.ExemptionUse{Amount: money.New(r.exempt, c), Month: *r.exemptMonth}
	}
	o.Direction, o.Status = domain.Direction(r.direction), domain.Status(r.status)
	o.Amount, o.Held, o.GMF = money.New(r.amount, c), money.New(r.held, c), money.New(r.gmf, c)
	return o, nil
}

// HoldOrder stores that o, which waited for compliance, is held: its status, its hold, the charges it covers, the
// day its debit was counted and what it used of the GMF exemption.
func (s Store) HoldOrder(ctx context.Context, o app.Order) error {
	feesJSON, err := feesOf(o.ID, o.Fees)
	if err != nil {
		return err
	}
	if _, err := s.q.Exec(ctx, `UPDATE payments.orders SET status = $2, hold_id = $3, held_units = $4, fees = $5, gmf_units = $6,
		debit_counted_on = $7, gmf_exempt_units = $8, gmf_exempt_month = $9 WHERE id = $1`, o.ID, o.Status, o.HoldID, o.Held.Units(), feesJSON,
		o.GMF.Units(), dateOrNull(o.DebitCountedOn), o.GMFExemption.Amount.Units(), dateOrNull(o.GMFExemption.Month)); err != nil {
		return fmt.Errorf("hold payment order %s: %w", o.ID, err)
	}
	return nil
}

// UpdateOrder stores where o stands: its status, why it was rejected, the rail's reference and its account, which
// only a transfer credited from suspense changes (the database refuses any other change of it).
func (s Store) UpdateOrder(ctx context.Context, o app.Order) error {
	if _, err := s.q.Exec(ctx, `UPDATE payments.orders SET status = $2, reason = nullif($3, ''), rail_ref = nullif($4, ''), account_id = $5,
		returned_at = CASE WHEN $2 = 'returned' THEN coalesce(returned_at, clock_timestamp()) END
		WHERE id = $1`, o.ID, o.Status, o.Reason, o.RailRef, orNull(o.AccountID)); err != nil {
		return fmt.Errorf("update payment order %s: %w", o.ID, err)
	}
	return nil
}
