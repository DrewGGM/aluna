//go:build integration

package ledger_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
)

const (
	evPosted   = "aluna.ledger.transaction.posted.v1"
	evReversed = "aluna.ledger.transaction.reversed.v1"
	evPlaced   = "aluna.ledger.hold.placed.v1"
	evCaptured = "aluna.ledger.hold.captured.v1"
	evReleased = "aluna.ledger.hold.released.v1"
	evExpired  = "aluna.ledger.hold.expired.v1"
)

type event struct {
	Type          string
	LegalEntityID ids.ID
	Data          map[string]any
}

// eventsAbout son los eventos del outbox sobre subject, en el orden en que se emitieron.
func eventsAbout(t *testing.T, subject string) []event {
	t.Helper()
	rows, err := testPool.Query(t.Context(), `SELECT type, legal_entity_id, payload FROM infra.outbox
		WHERE subject = $1 ORDER BY created_at, id`, subject)
	if err != nil {
		t.Fatal(err)
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (event, error) {
		var e event
		var payload []byte
		if err := row.Scan(&e.Type, &e.LegalEntityID, &payload); err != nil {
			return event{}, err
		}
		dec := json.NewDecoder(bytes.NewReader(payload))
		dec.UseNumber() // los montos se comparan como el texto del JSON, sin pasar por float
		return e, dec.Decode(&e.Data)
	})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// expectTypes revisa que los eventos sobre subject sean, en orden, los de los tipos want, y los devuelve.
func expectTypes(t *testing.T, subject string, want ...string) []event {
	t.Helper()
	events := eventsAbout(t, subject)
	var got []string
	for _, e := range events {
		got = append(got, e.Type)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("eventos de %s = %v, se esperaba %v", subject, got, want)
	}
	return events
}

// expectEntity revisa que todos los eventos sean de la entidad legal want.
func expectEntity(t *testing.T, events []event, want ids.ID) {
	t.Helper()
	for _, e := range events {
		if e.LegalEntityID != want {
			t.Errorf("%s de la entidad %s, se esperaba %s", e.Type, e.LegalEntityID, want)
		}
	}
}

// heldElsewhere pone un hold de una hora sobre un depósito con fondos de otra entidad, y devuelve el hold
// y la entidad.
func heldElsewhere(t *testing.T) (hold, other ids.ID) {
	t.Helper()
	other = ids.New()
	account := func(kind ledger.Kind) ledger.Account {
		a, err := tryCreateAccount(t, ledger.CreateAccountInput{
			LegalEntityID: other, Code: "cta-" + ids.New().String(), Kind: kind, Currency: cop(t), Mode: ledger.BalanceModeSync,
		})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	cash, deposit, clearing := account(ledger.KindAsset), account(ledger.KindLiability), account(ledger.KindLiability)
	in := transfer(t, cash.ID, deposit.ID, ids.New().String())
	in.LegalEntityID = other
	mustPostRef(t, in)
	h, err := inTx(t, func(tx postgres.Tx) (ledger.Hold, error) {
		return svc().PlaceHold(t.Context(), tx, ledger.PlaceHoldInput{
			LegalEntityID: other, AccountID: deposit.ID, CounterAccountID: clearing.ID, Amount: money.New(10, cop(t)), ExpiresAt: time.Now().Add(time.Hour),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return h.ID(), other
}

func txSubject(id ids.ID) string   { return "transactions/" + id.String() }
func holdSubject(id ids.ID) string { return "holds/" + id.String() }

// TestEachOperationEmitsOneEvent (FR-12): Post, Reverse, PlaceHold, Capture, Release y Expire emiten cada
// uno exactamente un evento de su tipo, sobre la transacción o el hold que cambiaron y con su entidad. El
// posting de un capture no emite transaction.posted: su evento es hold.captured. El lote de expiración
// mezcla entidades: cada hold.expired sale con la del hold.
func TestEachOperationEmitsOneEvent(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	original, _ := mustPostRef(t, transfer(t, createAccount(t, ledger.KindAsset).ID, deposit.ID, ids.New().String()))
	reversal, err := reverse(t, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	toCapture := mustPlaceHold(t, deposit.ID, clearing.ID, 10, time.Hour)
	capture, err := captureHold(t, toCapture, 10)
	if err != nil {
		t.Fatal(err)
	}
	toRelease := mustPlaceHold(t, deposit.ID, clearing.ID, 10, time.Hour)
	if _, err := releaseHold(t, toRelease); err != nil {
		t.Fatal(err)
	}
	toExpire := mustPlaceHold(t, deposit.ID, clearing.ID, 10, time.Hour)
	elsewhere, other := heldElsewhere(t)
	expireDue(t, afterTheHolds(), 100)

	expectEntity(t, expectTypes(t, txSubject(original.ID), evPosted), entity)
	reversals := expectTypes(t, txSubject(reversal.ID), evReversed)
	expectEntity(t, reversals, entity)
	if got := reversals[0].Data["reverses_id"]; got != original.ID.String() {
		t.Errorf("reverses_id = %v, se esperaba %s", got, original.ID)
	}
	expectEntity(t, expectTypes(t, holdSubject(toCapture), evPlaced, evCaptured), entity)
	expectTypes(t, txSubject(capture.tx.ID))
	expectEntity(t, expectTypes(t, holdSubject(toRelease), evPlaced, evReleased), entity)
	expectEntity(t, expectTypes(t, holdSubject(toExpire), evPlaced, evExpired), entity)
	expectEntity(t, expectTypes(t, holdSubject(elsewhere), evPlaced, evExpired), other)
}

// TestARepeatedOperationEmitsNothing: un posting repetido (FR-11) y una reversa que ya existe devuelven
// lo que ya estaba sin emitir otra vez.
func TestARepeatedOperationEmitsNothing(t *testing.T) {
	deposit, _ := fundedDeposit(t)
	in := transfer(t, createAccount(t, ledger.KindAsset).ID, deposit.ID, ids.New().String())
	original, _ := mustPostRef(t, in)
	if _, duplicate := mustPostRef(t, in); !duplicate {
		t.Fatal("el segundo posting con la misma referencia no se tomó como repetido")
	}
	reversal, err := reverse(t, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reverse(t, original.ID); !errors.Is(err, ledger.ErrAlreadyReversed) {
		t.Fatalf("segunda reversa = %v, se esperaba ErrAlreadyReversed", err)
	}

	expectTypes(t, txSubject(original.ID), evPosted)
	expectTypes(t, txSubject(reversal.ID), evReversed)
}

// TestARolledBackOperationEmitsNothing: el evento va en la transacción del comando; si esta se revierte,
// no queda.
func TestARolledBackOperationEmitsNothing(t *testing.T) {
	deposit, _ := fundedDeposit(t)
	cash := createAccount(t, ledger.KindAsset)
	errRollback := errors.New("se revierte a propósito")
	var postedID ids.ID
	_, err := inTx(t, func(tx postgres.Tx) (struct{}, error) {
		p, err := svc().Post(t.Context(), tx, transfer(t, cash.ID, deposit.ID, ids.New().String()))
		postedID = p.ID
		if err != nil {
			return struct{}{}, err
		}
		return struct{}{}, errRollback
	})

	if !errors.Is(err, errRollback) {
		t.Fatalf("posting = %v, se esperaba el rollback", err)
	}
	expectTypes(t, txSubject(postedID))
}

// TestTheEventsCarryIDsAmountsAndDates: el payload de un posting lleva su transacción, fechas y entries
// (cuenta, monto en unidades menores y moneda), sin actor, canal ni metadata; el de un capture, el hold
// con lo capturado y la transacción que lo asentó. Los dos son de la entidad del comando.
func TestTheEventsCarryIDsAmountsAndDates(t *testing.T) {
	deposit, clearing := fundedDeposit(t)
	cash := createAccount(t, ledger.KindAsset)
	in := transfer(t, cash.ID, deposit.ID, ids.New().String())
	in.Metadata = map[string]string{"nombre": "no va al evento"}
	tx, _ := mustPostRef(t, in)
	hold := mustPlaceHold(t, deposit.ID, clearing.ID, 30, time.Hour)
	capture, err := captureHold(t, hold, 20)
	if err != nil {
		t.Fatal(err)
	}

	got := expectTypes(t, txSubject(tx.ID), evPosted)[0]
	day := in.BusinessDate.Format(time.DateOnly)
	want := map[string]any{
		"transaction_id": tx.ID.String(), "code": "test", "source": "test", "external_ref": in.ExternalRef,
		"value_date": day, "business_date": day, "posted_at": tx.PostedAt.UTC().Format(time.RFC3339Nano),
		"entries": []any{
			map[string]any{"account_id": cash.ID.String(), "amount": map[string]any{"amount": json.Number("100"), "currency": "COP"}},
			map[string]any{"account_id": deposit.ID.String(), "amount": map[string]any{"amount": json.Number("-100"), "currency": "COP"}},
		},
	}
	if got.LegalEntityID != entity || !reflect.DeepEqual(got.Data, want) {
		t.Errorf("transaction.posted de %s = %v, se esperaba %v", got.LegalEntityID, got.Data, want)
	}
	gotHold := expectTypes(t, holdSubject(hold), evPlaced, evCaptured)[1]
	wantHold := map[string]any{
		"hold_id": hold.String(), "account_id": deposit.ID.String(), "counter_account_id": clearing.ID.String(),
		"amount": map[string]any{"amount": json.Number("30"), "currency": "COP"}, "captured": map[string]any{"amount": json.Number("20"), "currency": "COP"},
		"status": string(ledger.HoldCaptured), "expires_at": capture.hold.ExpiresAt().UTC().Format(time.RFC3339Nano),
		"transaction_id": capture.tx.ID.String(),
	}
	if gotHold.LegalEntityID != entity || !reflect.DeepEqual(gotHold.Data, wantHold) {
		t.Errorf("hold.captured de %s = %v, se esperaba %v", gotHold.LegalEntityID, gotHold.Data, wantHold)
	}
}
