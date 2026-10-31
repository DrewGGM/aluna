//go:build integration

package ledger_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// registeredEntity registers in the calendar an entity with business date 2025-07-07, June 2025 closed
// and May closing, and creates a cash account and a deposit for it. Entities and periods are written by
// whoever operates the deployment: the test switches back to the tables' owner.
func registeredEntity(t *testing.T) (id ids.ID, cash, deposit ledger.Account) {
	t.Helper()
	id = ids.New()
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `INSERT INTO calendar.legal_entities (id, name, business_date, functional_currency)
			VALUES ($1, 'Entity', '2025-07-07', 'COP')`, id); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), `INSERT INTO calendar.periods (legal_entity_id, period, status, closed_at, closed_by) VALUES
			($1, '2025-06', 'closed', clock_timestamp(), 'accountant'), ($1, '2025-05', 'closing', NULL, NULL)`, id)
		return err
	})
	return id, accountOf(t, id, ledger.KindAsset), accountOf(t, id, ledger.KindLiability)
}

// depositOn is a deposit of 100 to the entity's cash account with business date businessDate (zero: no
// date).
func depositOn(t *testing.T, entity ids.ID, cash, deposit ledger.Account, businessDate time.Time) ledger.PostInput {
	t.Helper()
	in := transfer(t, cash.ID, deposit.ID, ids.New().String())
	in.LegalEntityID, in.BusinessDate = entity, businessDate
	return in
}

// in2025 is day d of month m of 2025, the year of registeredEntity's calendar.
func in2025(m time.Month, d int) time.Time { return time.Date(2025, m, d, 0, 0, 0, 0, time.UTC) }

// TestAPostingInAClosedPeriodIsRejected (FR-15): with a business date in June, closed, the posting is
// ErrPeriodClosed and leaves nothing; in May (closing), July (open) and August (no row) it goes through.
func TestAPostingInAClosedPeriodIsRejected(t *testing.T) {
	entity, cash, deposit := registeredEntity(t)

	_, _, err := postRef(t, depositOn(t, entity, cash, deposit, in2025(time.June, 30)))

	if !errors.Is(err, ledger.ErrPeriodClosed) {
		t.Fatalf("posting in June = %v, want ErrPeriodClosed", err)
	}
	if b := balanceOf(t, cash.ID); b.Posted != 0 {
		t.Fatalf("balance = %+v, the rejected posting should not have posted", b)
	}
	for _, d := range []time.Time{in2025(time.May, 30), in2025(time.July, 7), in2025(time.August, 1)} {
		if _, _, err := postRef(t, depositOn(t, entity, cash, deposit, d)); err != nil {
			t.Errorf("posting on %s = %v, the period is not closed", d.Format(time.DateOnly), err)
		}
	}
}

// TestAPostingWithoutBusinessDateTakesTheEntitys: without a business date, the posting uses the entity's.
func TestAPostingWithoutBusinessDateTakesTheEntitys(t *testing.T) {
	entity, cash, deposit := registeredEntity(t)

	posted, _ := mustPostRef(t, depositOn(t, entity, cash, deposit, time.Time{}))

	if !posted.BusinessDate.Equal(in2025(time.July, 7)) {
		t.Fatalf("business date = %v, want the entity's (2025-07-07)", posted.BusinessDate)
	}
}

// TestWithoutBusinessDateAnUnregisteredEntityFails: the package's entity is not in the calendar, so
// without a business date there is nowhere to take it from.
func TestWithoutBusinessDateAnUnregisteredEntityFails(t *testing.T) {
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)

	_, _, err := postRef(t, depositOn(t, entity, cash, deposit, time.Time{}))

	if !errors.Is(err, ledger.ErrLegalEntityNotFound) {
		t.Fatalf("dateless posting of an entity without a calendar = %v, want ErrLegalEntityNotFound", err)
	}
}

// TestReversalsAndCapturesInAClosedPeriodAreRejected: the guard applies to everything the ledger posts.
func TestReversalsAndCapturesInAClosedPeriodAreRejected(t *testing.T) {
	entity, cash, deposit := registeredEntity(t)
	original, _ := mustPostRef(t, depositOn(t, entity, cash, deposit, in2025(time.July, 7)))
	june := in2025(time.June, 30)

	_, err := tryReverse(t, ledger.ReverseInput{LegalEntityID: entity, TransactionID: original.ID, Reason: reason, Actor: "operator", BusinessDate: june})
	if !errors.Is(err, ledger.ErrPeriodClosed) {
		t.Errorf("reversal in June = %v, want ErrPeriodClosed", err)
	}
	if b := balanceOf(t, cash.ID); b.Posted != 100 {
		t.Errorf("cash = %+v, the rejected reversal should not have posted", b)
	}

	hold, err := inTx(t, func(tx postgres.Tx) (ledger.Hold, error) {
		return svc().PlaceHold(t.Context(), tx, ledger.PlaceHoldInput{
			LegalEntityID: entity, AccountID: deposit.ID, CounterAccountID: cash.ID, Amount: money.New(50, cop(t)), ExpiresAt: time.Now().Add(time.Hour),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = inTx(t, func(tx postgres.Tx) (ledger.Transaction, error) {
		_, posted, err := svc().CaptureHold(t.Context(), tx, ledger.CaptureInput{
			LegalEntityID: entity, HoldID: hold.ID(), Amount: money.New(50, cop(t)), BusinessDate: june, Actor: "tester", Channel: "test",
		})
		return posted, err
	})
	if !errors.Is(err, ledger.ErrPeriodClosed) {
		t.Errorf("capture in June = %v, want ErrPeriodClosed", err)
	}
	if b := balanceOf(t, deposit.ID); b.PendingDebits != 50 || b.Posted != -100 {
		t.Errorf("deposit = %+v, the rejected capture should neither post nor release the hold", b)
	}
}

// closePeriod closes the entity's yyyy-mm period, as the close will (T-069).
func closePeriod(t *testing.T, entity ids.ID, period string) {
	t.Helper()
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO calendar.periods (legal_entity_id, period, status, closed_at, closed_by)
			VALUES ($1, $2, 'closed', clock_timestamp(), 'accountant')`, entity, period)
		return err
	})
}

// TestARetryAfterTheCloseReturnsTheOriginal (FR-11): what was posted in April stays idempotent after
// April is closed: the posting's retry returns the original and the reversal's, the reversal.
func TestARetryAfterTheCloseReturnsTheOriginal(t *testing.T) {
	entity, cash, deposit := registeredEntity(t)
	april := in2025(time.April, 30)
	in := depositOn(t, entity, cash, deposit, april)
	original, _ := mustPostRef(t, in)
	reverseApril := ledger.ReverseInput{LegalEntityID: entity, TransactionID: original.ID, Reason: reason, Actor: "operator", BusinessDate: april}
	reversal, err := tryReverse(t, reverseApril)
	if err != nil {
		t.Fatal(err)
	}
	closePeriod(t, entity, "2025-04")

	if again, duplicate := mustPostRef(t, in); !duplicate || again.ID != original.ID {
		t.Errorf("posting retry = %s (duplicate %v), want the original %s", again.ID, duplicate, original.ID)
	}
	if again, err := tryReverse(t, reverseApril); !errors.Is(err, ledger.ErrAlreadyReversed) || again.ID != reversal.ID {
		t.Errorf("reversal retry = %s, %v; want the reversal %s with ErrAlreadyReversed", again.ID, err, reversal.ID)
	}
}

// TestPeriodStatusOfTheCalendar: the calendar facade gives each month's status (no row, open). It lives
// here to reuse registeredEntity until there is a shared seeding helper (Q-74).
func TestPeriodStatusOfTheCalendar(t *testing.T) {
	entity, _, _ := registeredEntity(t)
	for d, want := range map[calendar.Date]calendar.PeriodStatus{
		{Year: 2025, Month: time.June, Day: 15}:  calendar.PeriodClosed,
		{Year: 2025, Month: time.May, Day: 15}:   calendar.PeriodClosing,
		{Year: 2025, Month: time.August, Day: 1}: calendar.PeriodOpen,
	} {
		if got, err := calendar.New().PeriodStatus(t.Context(), testPool, entity, d); err != nil || got != want {
			t.Errorf("PeriodStatus(%v) = %q, %v; want %q", d, got, err, want)
		}
	}
}
