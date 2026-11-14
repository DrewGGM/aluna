package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/money"
)

// message is a transfer of 50,000 COP.
func message(t *testing.T) OutboundMessage {
	t.Helper()
	cop, err := money.CurrencyOf("COP")
	if err != nil {
		t.Fatal(err)
	}
	return OutboundMessage{
		EndToEndID: "ALUNA1", InstrID: "INSTR1", TxID: "TX1", Amount: money.New(5000000, cop),
		Debtor: AccountRef{Number: "123456789"}, Creditor: AccountRef{Key: "@beneficiary"},
	}
}

// deaf is a rail that keeps the deadline its Send got and, with ended, does not return until ended closes,
// whatever its context says, or 5 seconds pass: a regression fails the test instead of hanging it.
type deaf struct {
	at    time.Time
	ended chan struct{}
}

func (d *deaf) Name() string { return "deaf" }

func (d *deaf) Outcome(Notification) (RailOutcome, error) { return RailOutcome{}, nil }

func (d *deaf) Transfer(Notification) (InboundTransfer, error) { return InboundTransfer{}, nil }

func (d *deaf) Send(ctx context.Context, _ OutboundMessage) (SendAck, error) {
	d.listen(ctx)
	return SendAck{}, nil
}

func (d *deaf) Return(ctx context.Context, _ ReturnMessage) error {
	d.listen(ctx)
	return nil
}

// listen notes the deadline the core gave and, if the rail holds, does not return until it is let go.
func (d *deaf) listen(ctx context.Context) {
	d.at, _ = ctx.Deadline()
	if d.ended != nil {
		select {
		case <-d.ended:
		case <-time.After(5 * time.Second):
		}
	}
}

// TestSendWaitsForTheRail15Seconds (FR-31): the core gives a rail 15 seconds to take a message, and stops
// waiting when its context ends even if the rail does not return.
func TestSendWaitsForTheRail15Seconds(t *testing.T) {
	rail := &deaf{}
	before := time.Now()
	if _, err := Send(t.Context(), rail, message(t)); err != nil {
		t.Fatal(err)
	}
	if waited := rail.at.Sub(before); waited < 15*time.Second || waited > 16*time.Second {
		t.Fatalf("the rail had %s, want 15 s", waited)
	}
	holding := &deaf{ended: make(chan struct{})}
	t.Cleanup(func() { close(holding.ended) })
	impatient, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := Send(impatient, holding, message(t))

	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("a rail that does not return = %v after %s, want DeadlineExceeded at once", err, time.Since(start))
	}
}

// TestAReturnWaitsForTheRailAsASend (FR-32): a return to the sender gets the same 15 seconds, and stops waiting when
// its context ends.
func TestAReturnWaitsForTheRailAsASend(t *testing.T) {
	rail := &deaf{}
	before := time.Now()
	if err := HandReturn(t.Context(), rail, ReturnMessage{EndToEndID: "E2E", DebtorAgent: "BANK", Reason: "AC01"}); err != nil {
		t.Fatal(err)
	}
	if waited := rail.at.Sub(before); waited < 15*time.Second || waited > 16*time.Second {
		t.Fatalf("the rail had %s, want 15 s", waited)
	}
	holding := &deaf{ended: make(chan struct{})}
	t.Cleanup(func() { close(holding.ended) })
	impatient, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	err := HandReturn(impatient, holding, ReturnMessage{EndToEndID: "E2E", DebtorAgent: "BANK", Reason: "AC01"})

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a rail that does not return = %v, want DeadlineExceeded", err)
	}
}
