//go:build integration

package payments_test

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/payments"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// returnedToSender returns the transfer in suspense in to its bank for AC01, approved by operator-b, and is its
// order.
func (w world) returnedToSender(t *testing.T, in payments.InboundTransfer) received {
	t.Helper()
	if err := w.receive(t, in); err != nil {
		t.Fatal(err)
	}
	o := w.only(t, in)
	mustDo(t, func(tx postgres.Tx) error {
		_, err := w.payments.ReturnSuspended(t.Context(), tx, w.entity, o.id, "AC01", "operator-b")
		return err
	})
	return o
}

// returnBooked are the units the return of a transfer to its bank posts to each account of the world's entity.
func (w world) returnBooked(t *testing.T) map[string]int64 {
	t.Helper()
	rows, err := testPool.Query(t.Context(), `SELECT a.code, e.amount
		FROM ledger.entries e JOIN ledger.transactions t ON t.id = e.transaction_id JOIN ledger.accounts a ON a.id = e.account_id
		WHERE t.legal_entity_id = $1 AND t.code = 'payments.return_to_sender'`, w.entity)
	if err != nil {
		t.Fatal(err)
	}
	units := map[string]int64{}
	var (
		account string
		amount  int64
	)
	if _, err := pgx.ForEachRow(rows, []any{&account, &amount}, func() error {
		units[account] += amount
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return units
}

// TestASuspenseIsReturnedToItsBank (FR-32): a transfer held in suspense that belongs to no customer goes back to the
// bank that sent it: its order is returned for the reason given, keeping when, which not even the owner changes, the
// money moves from the suspense account back to the rail's clearing account, and the rail gets a return of its
// end-to-end id and amount to its debtor agent.
func TestASuspenseIsReturnedToItsBank(t *testing.T) {
	w := newWorld(t, true)
	in := w.transfer(t, 30000000, payments.AccountRef{Key: "@nobody"})
	o := w.returnedToSender(t, in)

	if err := w.payments.HandReturn(t.Context(), postgres.NewUnitOfWork(testPool), o.id); err != nil {
		t.Fatal(err)
	}

	if now := w.only(t, in); now.status != "returned" || now.reason != "AC01" {
		t.Fatalf("the transfer = %+v, want it returned as AC01", now)
	}
	var stamped bool
	if err := testPool.QueryRow(t.Context(), `SELECT returned_at IS NOT NULL FROM payments.orders WHERE id = $1`, o.id).Scan(&stamped); err != nil || !stamped {
		t.Fatalf("the transfer keeps when it was returned: %t (%v), want it kept", stamped, err)
	}
	if got := sqlState(asOwner(t, `UPDATE payments.orders SET returned_at = returned_at - interval '1 day' WHERE id = $1`, o.id)); got != "2F003 " {
		t.Errorf("the owner changing when it was returned = %q, want 2F003", got)
	}
	if units, want := w.returnBooked(t), map[string]int64{"L.SUSPENSE.SIMBREB": 30000000, "L.CLEARING.SIMBREB": -30000000}; !maps.Equal(units, want) {
		t.Fatalf("booked %v, want the amount out of suspense and into the clearing account", units)
	}
	want := payments.ReturnMessage{EndToEndID: in.EndToEndID, DebtorAgent: in.DebtorAgent, Amount: in.Amount, Reason: "AC01"}
	if got := w.rail.returns(); len(got) != 1 || got[0] != want {
		t.Fatalf("returns handed to the rail = %+v, want %+v", got, want)
	}
}

// TestAReturnWithoutAnswerIsNotHandedAgain (FR-32): a return the rail did not answer may have reached it: it is
// ErrNoAnswer, which the job cancels instead of handing it again; one the rail refused is ErrRailRefused.
func TestAReturnWithoutAnswerIsNotHandedAgain(t *testing.T) {
	w := newWorld(t, true)
	o := w.returnedToSender(t, w.transfer(t, 30000000, payments.AccountRef{Key: "@nobody"}))
	uow := postgres.NewUnitOfWork(testPool)

	w.rail.answer = func() error { return context.DeadlineExceeded }
	if err := w.payments.HandReturn(t.Context(), uow, o.id); !errors.Is(err, payments.ErrNoAnswer) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a return without answer = %v, want ErrNoAnswer", err)
	}
	w.rail.answer = func() error { return payments.ErrRailRefused }
	if err := w.payments.HandReturn(t.Context(), uow, o.id); !errors.Is(err, payments.ErrRailRefused) || errors.Is(err, payments.ErrNoAnswer) {
		t.Fatalf("a refused return = %v, want ErrRailRefused", err)
	}
}
