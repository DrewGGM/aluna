// Package postgres stores payment orders in payments.orders.
package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/payments/internal/app"
	"github.com/DrewGGM/aluna/internal/postgres"
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
