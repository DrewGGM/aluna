package domain

import (
	"errors"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
)

var (
	holdPlacedAt  = time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	holdExpiresAt = holdPlacedAt.Add(24 * time.Hour)
)

// fataler is what *testing.T and *rapid.T have in common.
type fataler interface {
	Helper()
	Fatalf(format string, args ...any)
}

func pendingHold(t fataler, units int64) Hold {
	t.Helper()
	cop, err := money.CurrencyOf("COP")
	if err != nil {
		t.Fatalf("CurrencyOf(COP): %v", err)
	}
	h, err := NewHold(NewHoldInput{ID: ids.New(), AccountID: ids.New(), Amount: money.New(units, cop), ExpiresAt: holdExpiresAt, Now: holdPlacedAt})
	if err != nil {
		t.Fatalf("NewHold: %v", err)
	}
	return h
}

func TestNewHold(t *testing.T) {
	cop := currency(t, "COP")
	valid := func() NewHoldInput {
		return NewHoldInput{ID: ids.New(), AccountID: ids.New(), Amount: money.New(100, cop), ExpiresAt: holdExpiresAt, Now: holdPlacedAt}
	}
	invalid := map[string]func(*NewHoldInput){
		"zero amount":         func(in *NewHoldInput) { in.Amount = money.New(0, cop) },
		"negative amount":     func(in *NewHoldInput) { in.Amount = money.New(-1, cop) },
		"no currency":         func(in *NewHoldInput) { in.Amount = money.New(1, money.Currency{}) },
		"no identifier":       func(in *NewHoldInput) { in.ID = ids.ID{} },
		"no account":          func(in *NewHoldInput) { in.AccountID = ids.ID{} },
		"expires on creation": func(in *NewHoldInput) { in.ExpiresAt = in.Now },
		"no creation instant": func(in *NewHoldInput) { in.Now = time.Time{} },
	}
	for name, change := range invalid {
		t.Run(name, func(t *testing.T) {
			in := valid()
			change(&in)
			if _, err := NewHold(in); !errors.Is(err, ErrInvalidHold) {
				t.Fatalf("NewHold = %v, want ErrInvalidHold", err)
			}
		})
	}
	in := valid()
	h, err := NewHold(in)
	want := Hold{id: in.ID, accountID: in.AccountID, amount: in.Amount, captured: money.New(0, cop), status: HoldPending, expiresAt: holdExpiresAt}
	if err != nil || h != want {
		t.Fatalf("NewHold = %+v, %v; want %+v", h, err, want)
	}
	if h.ID() != in.ID || h.AccountID() != in.AccountID || h.Amount() != in.Amount || !h.ExpiresAt().Equal(holdExpiresAt) {
		t.Fatalf("the getters do not return what was created: %+v", h)
	}
}

// holdOp is an operation on a hold, for the transition table.
type holdOp func(Hold) (Hold, error)

func captureOp(units int64, now time.Time) holdOp {
	return func(h Hold) (Hold, error) { h, _, err := h.Capture(units, now); return h, err }
}

func releaseOp(h Hold) (Hold, error) { return h.Release() }

func expireOp(now time.Time) holdOp { return func(h Hold) (Hold, error) { return h.Expire(now) } }

type transition struct {
	name    string
	from    HoldStatus
	op      holdOp
	want    HoldStatus // status after an accepted transition
	wantErr error
}

// holdTransitions are the FR-6 transitions: pending → captured | released | expired, the rejections from
// pending and, from each terminal status, each operation (all rejected). Operations on a finished hold
// run after the expiry: a hold that expired while its capture was arriving answers ErrHoldNotPending
// (PRD, edge cases), not ErrHoldExpired.
func holdTransitions() []transition {
	cases := []transition{
		{"pending → captured (full)", HoldPending, captureOp(100, holdPlacedAt), HoldCaptured, nil},
		{"pending → captured (partial)", HoldPending, captureOp(40, holdPlacedAt), HoldCaptured, nil},
		{"pending → captured right at expiry", HoldPending, captureOp(100, holdExpiresAt), HoldCaptured, nil},
		{"pending → released", HoldPending, releaseOp, HoldReleased, nil},
		{"pending → expired", HoldPending, expireOp(holdExpiresAt.Add(time.Second)), HoldExpired, nil},
		{"capture too much", HoldPending, captureOp(101, holdPlacedAt), "", ErrAmountExceedsHold},
		{"capture zero", HoldPending, captureOp(0, holdPlacedAt), "", ErrInvalidHold},
		{"capture negative", HoldPending, captureOp(-1, holdPlacedAt), "", ErrInvalidHold},
		{"capture expired", HoldPending, captureOp(100, holdExpiresAt.Add(time.Second)), "", ErrHoldExpired},
		{"expire right at expiry", HoldPending, expireOp(holdExpiresAt), "", ErrHoldNotDue},
	}
	late := holdExpiresAt.Add(time.Hour)
	ops := []struct {
		name string
		op   holdOp
	}{{"capture", captureOp(1, late)}, {"release", releaseOp}, {"expire", expireOp(late)}}
	for _, from := range []HoldStatus{HoldCaptured, HoldReleased, HoldExpired} {
		for _, o := range ops {
			cases = append(cases, transition{string(from) + " → " + o.name, from, o.op, "", ErrHoldNotPending})
		}
	}
	return cases
}

func TestHoldTransitions(t *testing.T) {
	toTerminal := map[HoldStatus]holdOp{
		HoldCaptured: captureOp(100, holdPlacedAt),
		HoldReleased: releaseOp,
		HoldExpired:  expireOp(holdExpiresAt.Add(time.Second)),
	}
	for _, tc := range holdTransitions() {
		t.Run(tc.name, func(t *testing.T) {
			h := pendingHold(t, 100)
			if tc.from != HoldPending {
				var err error
				if h, err = toTerminal[tc.from](h); err != nil {
					t.Fatal(err)
				}
			}

			got, err := tc.op(h)

			switch {
			case !errors.Is(err, tc.wantErr):
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			case err != nil && got != (Hold{}):
				t.Fatalf("a rejected transition returned %+v; want the zero value", got)
			case err == nil && got.Status() != tc.want:
				t.Fatalf("status = %s, want %s", got.Status(), tc.want)
			}
		})
	}
}

// TestPartialCaptureReleasesTheRest (FR-6): what is not captured is released.
func TestPartialCaptureReleasesTheRest(t *testing.T) {
	h := pendingHold(t, 100)
	cop := h.Amount().Currency()

	captured, result, err := h.Capture(40, holdPlacedAt)

	want := CaptureResult{Captured: money.New(40, cop), Released: money.New(60, cop)}
	if err != nil || result != want || captured.Captured() != money.New(40, cop) || captured.Amount() != h.Amount() {
		t.Fatalf("Capture(40) of 100 = %+v, %+v, %v; want 40 captured and 60 released", captured, result, err)
	}
}

// TestHoldReachesAtMostOneTerminalState (property): in any sequence of operations, an accepted operation
// takes the hold out of pending and leaves it finished; a finished hold rejects everything with
// ErrHoldNotPending and no longer changes; the held amount never changes, and the captured and released
// amounts add up to the held amount.
func TestHoldReachesAtMostOneTerminalState(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		units := rapid.Int64Range(1, 1_000_000).Draw(t, "amount")
		h := pendingHold(t, units)
		for i := range rapid.IntRange(1, 10).Draw(t, "operations") {
			now := holdPlacedAt.Add(time.Duration(rapid.Int64Range(0, 48).Draw(t, "hour")) * time.Hour)
			next, result, err := step(t, h, now)
			if h.Status() != HoldPending {
				if !errors.Is(err, ErrHoldNotPending) {
					t.Fatalf("operation %d: a %s hold accepted an operation (%v)", i, h.Status(), err)
				}
				continue
			}
			if err != nil {
				continue
			}
			checkTerminal(t, h, next, result)
			h = next
		}
	})
}

// step applies a random operation. result only applies to an accepted capture.
func step(t *rapid.T, h Hold, now time.Time) (Hold, CaptureResult, error) {
	switch rapid.IntRange(0, 2).Draw(t, "op") {
	case 0:
		return h.Capture(rapid.Int64Range(-1, h.Amount().Units()+1).Draw(t, "capture"), now)
	case 1:
		next, err := h.Release()
		return next, CaptureResult{}, err
	default:
		next, err := h.Expire(now)
		return next, CaptureResult{}, err
	}
}

// checkTerminal checks an accepted transition from pending.
func checkTerminal(t *rapid.T, before, after Hold, result CaptureResult) {
	t.Helper()
	switch {
	case after.Status() == HoldPending:
		t.Fatalf("an accepted operation left the hold pending")
	case after.Amount() != before.Amount():
		t.Fatalf("the held amount changed from %v to %v", before.Amount(), after.Amount())
	case after.Status() != HoldCaptured && !after.Captured().IsZero():
		t.Fatalf("a %s hold was left with %v captured", after.Status(), after.Captured())
	case after.Status() == HoldCaptured && (after.Captured() != result.Captured ||
		result.Captured.Units()+result.Released.Units() != before.Amount().Units()):
		t.Fatalf("captured %v and released %v do not add up to %v", result.Captured, result.Released, before.Amount())
	}
}

func TestRestoreHold(t *testing.T) {
	cop, usd := currency(t, "COP"), currency(t, "USD")
	snapshot := func(status HoldStatus, captured int64) HoldSnapshot {
		return HoldSnapshot{
			ID: ids.New(), AccountID: ids.New(), Amount: money.New(100, cop), Captured: money.New(captured, cop),
			Status: status, ExpiresAt: holdExpiresAt,
		}
	}
	for _, s := range []HoldSnapshot{snapshot(HoldPending, 0), snapshot(HoldCaptured, 40), snapshot(HoldReleased, 0), snapshot(HoldExpired, 0)} {
		h, err := RestoreHold(s)
		if err != nil || h.ID() != s.ID || h.Status() != s.Status || h.Captured() != s.Captured || h.Amount() != s.Amount {
			t.Errorf("RestoreHold(%+v) = %+v, %v", s, h, err)
		}
	}
	otherCurrency := snapshot(HoldCaptured, 40)
	otherCurrency.Captured = money.New(40, usd)
	noExpiry := snapshot(HoldPending, 0)
	noExpiry.ExpiresAt = time.Time{}
	zeroAmount := snapshot(HoldPending, 0)
	zeroAmount.Amount = money.New(0, cop)
	noID := snapshot(HoldPending, 0)
	noID.ID = ids.ID{}
	invalid := map[string]HoldSnapshot{
		"unknown status":                 snapshot("frozen", 0),
		"captured with nothing captured": snapshot(HoldCaptured, 0),
		"pending with captured amount":   snapshot(HoldPending, 10),
		"captured too much":              snapshot(HoldCaptured, 101),
		"negative captured amount":       snapshot(HoldReleased, -1),
		"captured in another currency":   otherCurrency,
		"no expiry":                      noExpiry,
		"zero amount":                    zeroAmount,
		"no identifier":                  noID,
	}
	for name, s := range invalid {
		if _, err := RestoreHold(s); !errors.Is(err, ErrInvalidHold) {
			t.Errorf("%s: RestoreHold = %v, want ErrInvalidHold", name, err)
		}
	}
}

// TestARestoredOverdueHoldIsCapturedAsExpired: a pending hold already past its expiry is restored (NewHold
// would not allow it) and capturing it gives ErrHoldExpired, while Expire closes it.
func TestARestoredOverdueHoldIsCapturedAsExpired(t *testing.T) {
	cop := currency(t, "COP")
	h, err := RestoreHold(HoldSnapshot{
		ID: ids.New(), AccountID: ids.New(), Amount: money.New(100, cop), Captured: money.New(0, cop),
		Status: HoldPending, ExpiresAt: holdExpiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	late := holdExpiresAt.Add(time.Minute)

	if _, _, err := h.Capture(100, late); !errors.Is(err, ErrHoldExpired) {
		t.Errorf("capture expired = %v, want ErrHoldExpired", err)
	}
	if expired, err := h.Expire(late); err != nil || expired.Status() != HoldExpired {
		t.Errorf("expire = %+v, %v", expired, err)
	}
}
