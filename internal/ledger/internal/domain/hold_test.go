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

// fataler es lo que comparten *testing.T y *rapid.T.
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
		"importe cero":             func(in *NewHoldInput) { in.Amount = money.New(0, cop) },
		"importe negativo":         func(in *NewHoldInput) { in.Amount = money.New(-1, cop) },
		"sin moneda":               func(in *NewHoldInput) { in.Amount = money.New(1, money.Currency{}) },
		"sin identificador":        func(in *NewHoldInput) { in.ID = ids.ID{} },
		"sin cuenta":               func(in *NewHoldInput) { in.AccountID = ids.ID{} },
		"vence al crearse":         func(in *NewHoldInput) { in.ExpiresAt = in.Now },
		"sin instante de creación": func(in *NewHoldInput) { in.Now = time.Time{} },
	}
	for name, change := range invalid {
		t.Run(name, func(t *testing.T) {
			in := valid()
			change(&in)
			if _, err := NewHold(in); !errors.Is(err, ErrInvalidHold) {
				t.Fatalf("NewHold = %v, se esperaba ErrInvalidHold", err)
			}
		})
	}
	in := valid()
	h, err := NewHold(in)
	want := Hold{id: in.ID, accountID: in.AccountID, amount: in.Amount, captured: money.New(0, cop), status: HoldPending, expiresAt: holdExpiresAt}
	if err != nil || h != want {
		t.Fatalf("NewHold = %+v, %v; se esperaba %+v", h, err, want)
	}
	if h.ID() != in.ID || h.AccountID() != in.AccountID || h.Amount() != in.Amount || !h.ExpiresAt().Equal(holdExpiresAt) {
		t.Fatalf("los getters no devuelven lo creado: %+v", h)
	}
}

// holdOp es una operación sobre un hold, para la tabla de transiciones.
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
	want    HoldStatus // estado tras una transición aceptada
	wantErr error
}

// holdTransitions son las transiciones de FR-6: pending → captured | released | expired, los
// rechazos desde pending y, desde cada estado terminal, cada operación (todas rechazadas). Las
// operaciones sobre un hold terminado van después del vencimiento: un hold que expiró mientras llegaba
// su capture responde ErrHoldNotPending (PRD, casos borde), no ErrHoldExpired.
func holdTransitions() []transition {
	cases := []transition{
		{"pending → captured (total)", HoldPending, captureOp(100, holdPlacedAt), HoldCaptured, nil},
		{"pending → captured (parcial)", HoldPending, captureOp(40, holdPlacedAt), HoldCaptured, nil},
		{"pending → captured justo al vencer", HoldPending, captureOp(100, holdExpiresAt), HoldCaptured, nil},
		{"pending → released", HoldPending, releaseOp, HoldReleased, nil},
		{"pending → expired", HoldPending, expireOp(holdExpiresAt.Add(time.Second)), HoldExpired, nil},
		{"capturar de más", HoldPending, captureOp(101, holdPlacedAt), "", ErrAmountExceedsHold},
		{"capturar cero", HoldPending, captureOp(0, holdPlacedAt), "", ErrInvalidHold},
		{"capturar negativo", HoldPending, captureOp(-1, holdPlacedAt), "", ErrInvalidHold},
		{"capturar vencido", HoldPending, captureOp(100, holdExpiresAt.Add(time.Second)), "", ErrHoldExpired},
		{"expirar justo al vencer", HoldPending, expireOp(holdExpiresAt), "", ErrHoldNotDue},
	}
	late := holdExpiresAt.Add(time.Hour)
	ops := []struct {
		name string
		op   holdOp
	}{{"capturar", captureOp(1, late)}, {"liberar", releaseOp}, {"expirar", expireOp(late)}}
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
				t.Fatalf("error = %v, se esperaba %v", err, tc.wantErr)
			case err != nil && got != (Hold{}):
				t.Fatalf("una transición rechazada devolvió %+v; se esperaba el valor cero", got)
			case err == nil && got.Status() != tc.want:
				t.Fatalf("estado = %s, se esperaba %s", got.Status(), tc.want)
			}
		})
	}
}

// TestPartialCaptureReleasesTheRest (FR-6): lo no capturado se libera.
func TestPartialCaptureReleasesTheRest(t *testing.T) {
	h := pendingHold(t, 100)
	cop := h.Amount().Currency()

	captured, result, err := h.Capture(40, holdPlacedAt)

	want := CaptureResult{Captured: money.New(40, cop), Released: money.New(60, cop)}
	if err != nil || result != want || captured.Captured() != money.New(40, cop) || captured.Amount() != h.Amount() {
		t.Fatalf("Capture(40) de 100 = %+v, %+v, %v; se esperaban 40 capturados y 60 liberados", captured, result, err)
	}
}

// TestHoldReachesAtMostOneTerminalState (property): en cualquier secuencia de operaciones, una
// operación aceptada saca al hold de pending y lo deja terminado; un hold terminado rechaza todo con
// ErrHoldNotPending y ya no cambia; lo retenido nunca cambia, y lo capturado y lo liberado suman lo
// retenido.
func TestHoldReachesAtMostOneTerminalState(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		units := rapid.Int64Range(1, 1_000_000).Draw(t, "importe")
		h := pendingHold(t, units)
		for i := range rapid.IntRange(1, 10).Draw(t, "operaciones") {
			now := holdPlacedAt.Add(time.Duration(rapid.Int64Range(0, 48).Draw(t, "hora")) * time.Hour)
			next, result, err := step(t, h, now)
			if h.Status() != HoldPending {
				if !errors.Is(err, ErrHoldNotPending) {
					t.Fatalf("operación %d: un hold %s aceptó una operación (%v)", i, h.Status(), err)
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

// step aplica una operación al azar. result solo vale para un capture aceptado.
func step(t *rapid.T, h Hold, now time.Time) (Hold, CaptureResult, error) {
	switch rapid.IntRange(0, 2).Draw(t, "op") {
	case 0:
		return h.Capture(rapid.Int64Range(-1, h.Amount().Units()+1).Draw(t, "capturar"), now)
	case 1:
		next, err := h.Release()
		return next, CaptureResult{}, err
	default:
		next, err := h.Expire(now)
		return next, CaptureResult{}, err
	}
}

// checkTerminal verifica una transición aceptada desde pending.
func checkTerminal(t *rapid.T, before, after Hold, result CaptureResult) {
	t.Helper()
	switch {
	case after.Status() == HoldPending:
		t.Fatalf("una operación aceptada dejó el hold en pending")
	case after.Amount() != before.Amount():
		t.Fatalf("lo retenido cambió de %v a %v", before.Amount(), after.Amount())
	case after.Status() != HoldCaptured && !after.Captured().IsZero():
		t.Fatalf("un hold %s quedó con %v capturado", after.Status(), after.Captured())
	case after.Status() == HoldCaptured && (after.Captured() != result.Captured ||
		result.Captured.Units()+result.Released.Units() != before.Amount().Units()):
		t.Fatalf("capturado %v y liberado %v no suman %v", result.Captured, result.Released, before.Amount())
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
		"estado desconocido":       snapshot("frozen", 0),
		"captured sin capturado":   snapshot(HoldCaptured, 0),
		"pending con capturado":    snapshot(HoldPending, 10),
		"capturado de más":         snapshot(HoldCaptured, 101),
		"capturado negativo":       snapshot(HoldReleased, -1),
		"capturado en otra moneda": otherCurrency,
		"sin vencimiento":          noExpiry,
		"importe cero":             zeroAmount,
		"sin identificador":        noID,
	}
	for name, s := range invalid {
		if _, err := RestoreHold(s); !errors.Is(err, ErrInvalidHold) {
			t.Errorf("%s: RestoreHold = %v, se esperaba ErrInvalidHold", name, err)
		}
	}
}

// TestARestoredOverdueHoldIsCapturedAsExpired: un hold pending ya vencido se reconstruye (NewHold no
// lo permitiría) y su capture es ErrHoldExpired, mientras que Expire lo cierra.
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
		t.Errorf("capturar vencido = %v, se esperaba ErrHoldExpired", err)
	}
	if expired, err := h.Expire(late); err != nil || expired.Status() != HoldExpired {
		t.Errorf("expirar = %+v, %v", expired, err)
	}
}
