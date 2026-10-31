package app

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger/internal/domain"
	"github.com/DrewGGM/aluna/internal/money"
)

// ref is the FR-11 uniqueness key.
type ref struct {
	entity         ids.ID
	source, extRef string
}

// fakeStore is an in-memory PostingStore and AccountWriter that records what was locked and what was
// written. posted are the transactions already posted. To simulate another producer with the same
// reference, whileLocking shows up when the balances are locked and whileInserting on insert.
type fakeStore struct {
	accounts       map[ids.ID]domain.Account
	balances       map[ids.ID]LockedBalance
	posted         map[ref]Transaction
	whileLocking   *Transaction
	whileInserting *Transaction
	locked         []ids.ID
	inserted       []TransactionRecord
	entries        []domain.EntryInput
	added          map[ids.ID]int64
}

func newFakeStore(accounts ...domain.Account) *fakeStore {
	s := &fakeStore{
		accounts: map[ids.ID]domain.Account{}, balances: map[ids.ID]LockedBalance{}, posted: map[ref]Transaction{},
		added: map[ids.ID]int64{},
	}
	for _, a := range accounts {
		s.accounts[a.ID] = a
		s.balances[a.ID] = LockedBalance{}
	}
	return s
}

func (s *fakeStore) InsertAccount(_ context.Context, a domain.Account) error {
	s.accounts[a.ID] = a
	return nil
}

func (s *fakeStore) Accounts(_ context.Context, accountIDs []ids.ID) (map[ids.ID]domain.Account, error) {
	found := map[ids.ID]domain.Account{}
	for _, id := range accountIDs {
		if a, ok := s.accounts[id]; ok {
			found[id] = a
		}
	}
	return found, nil
}

func (s *fakeStore) LockBalances(_ context.Context, accountIDs []ids.ID) (map[ids.ID]LockedBalance, error) {
	s.locked = append(s.locked, accountIDs...)
	if s.whileLocking != nil {
		s.posted[refOf(s.whileLocking.TransactionRecord)] = *s.whileLocking
	}
	found := map[ids.ID]LockedBalance{}
	for _, id := range accountIDs {
		if b, ok := s.balances[id]; ok {
			found[id] = b
		}
	}
	return found, nil
}

func refOf(rec TransactionRecord) ref { return ref{rec.LegalEntityID, rec.Source, rec.ExternalRef} }

func (s *fakeStore) TransactionByRef(_ context.Context, entity ids.ID, source, extRef string) (Transaction, bool, error) {
	t, ok := s.posted[ref{entity, source, extRef}]
	return t, ok, nil
}

func (s *fakeStore) InsertTransaction(_ context.Context, rec TransactionRecord, entries []domain.EntryInput) (time.Time, error) {
	if s.whileInserting != nil {
		s.posted[refOf(rec)] = *s.whileInserting
		return time.Time{}, ErrDuplicateExternalRef
	}
	s.inserted = append(s.inserted, rec)
	s.entries = append(s.entries, entries...)
	return time.Unix(1_800_000_000, 0), nil
}

func (s *fakeStore) AddToBalance(_ context.Context, accountID ids.ID, d BalanceDelta) error {
	s.added[accountID] += d.Posted
	return nil
}

var entity = ids.New()

// openPeriods is a calendar with every period open: the integration tests cover the guard.
type openPeriods struct{}

func (openPeriods) EnsureOpen(context.Context, ids.ID, time.Time) error { return nil }

// noEvents discards the events: events_integration_test.go covers what each use case emits.
type noEvents struct{}

func (noEvents) Emit(context.Context, Event) error { return nil }

func cop(t *testing.T) money.Currency {
	t.Helper()
	c, err := money.CurrencyOf("COP")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func account(t *testing.T, kind domain.Kind, mode domain.BalanceMode) domain.Account {
	t.Helper()
	return domain.Account{ID: ids.New(), LegalEntityID: entity, Code: "c", Kind: kind, Currency: cop(t), Mode: mode}
}

func input(entries ...domain.EntryInput) PostInput {
	today := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	return PostInput{
		LegalEntityID: entity, Code: "test", ValueDate: today, BusinessDate: today, Source: "test",
		ExternalRef: ids.New().String(), Actor: "tester", Channel: "test", RuleVersion: "v1", Entries: entries,
	}
}

func entry(a domain.Account, units int64) domain.EntryInput {
	return domain.EntryInput{AccountID: a.ID, Amount: money.New(units, a.Currency)}
}

func TestPostNetsEntriesPerAccountAndLocksEachOnce(t *testing.T) {
	cash, deposit := account(t, domain.KindAsset, domain.BalanceModeSync), account(t, domain.KindLiability, domain.BalanceModeSync)
	store := newFakeStore(cash, deposit)

	_, err := Post(t.Context(), store, openPeriods{}, noEvents{}, input(entry(cash, 70), entry(cash, 30), entry(deposit, -100)))
	if err != nil {
		t.Fatal(err)
	}
	if store.added[cash.ID] != 100 || store.added[deposit.ID] != -100 {
		t.Errorf("deltas = %v, want +100 on cash and -100 on the deposit", store.added)
	}
	if len(store.locked) != 2 || len(store.entries) != 3 {
		t.Errorf("locks = %v, entries = %d: want once per account and the three entries", store.locked, len(store.entries))
	}
}

// TestPostChecksTheNetNotEachEntry: an equal debit and credit on an account already below its limit do
// not make it worse; checking entry by entry would reject it.
func TestPostChecksTheNetNotEachEntry(t *testing.T) {
	cash, deposit := account(t, domain.KindAsset, domain.BalanceModeSync), account(t, domain.KindLiability, domain.BalanceModeSync)
	store := newFakeStore(cash, deposit)
	store.balances[deposit.ID] = LockedBalance{Posted: 50} // natural -50: below its limit of 0

	_, err := Post(t.Context(), store, openPeriods{}, noEvents{}, input(entry(deposit, 10), entry(deposit, -10), entry(cash, 5), entry(cash, -5)))
	if err != nil {
		t.Fatalf("a zero net does not reduce the balance: %v", err)
	}
}

func TestPostSkipsAsyncAccounts(t *testing.T) {
	deposit, clearing := account(t, domain.KindLiability, domain.BalanceModeSync), account(t, domain.KindAsset, domain.BalanceModeAsync)
	store := newFakeStore(deposit, clearing)

	if _, err := Post(t.Context(), store, openPeriods{}, noEvents{}, input(entry(clearing, 100), entry(deposit, -100))); err != nil {
		t.Fatal(err)
	}

	if slices.Contains(store.locked, clearing.ID) || store.added[clearing.ID] != 0 {
		t.Errorf("the async account is neither locked nor updated here: locks %v, deltas %v", store.locked, store.added)
	}
}

func TestPostRejectsBeforeWriting(t *testing.T) {
	cash, deposit := account(t, domain.KindAsset, domain.BalanceModeSync), account(t, domain.KindLiability, domain.BalanceModeSync)
	foreign := account(t, domain.KindLiability, domain.BalanceModeSync)
	foreign.LegalEntityID = ids.New()
	usd, err := money.CurrencyOf("USD")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		entries []domain.EntryInput
		want    error
	}{
		"unbalanced":                 {[]domain.EntryInput{entry(cash, 100), entry(deposit, -99)}, domain.ErrUnbalanced},
		"another entity's account":   {[]domain.EntryInput{entry(cash, 100), entry(foreign, -100)}, ErrAccountNotFound},
		"nonexistent account":        {[]domain.EntryInput{entry(cash, 100), {AccountID: ids.New(), Amount: money.New(-100, cop(t))}}, ErrAccountNotFound},
		"another account's currency": {[]domain.EntryInput{{AccountID: cash.ID, Amount: money.New(100, usd)}, {AccountID: deposit.ID, Amount: money.New(-100, usd)}}, ErrCurrencyMismatch},
		"overdraft":                  {[]domain.EntryInput{entry(deposit, 100), entry(cash, -100)}, domain.ErrInsufficientFunds},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := newFakeStore(cash, deposit, foreign)

			_, err := Post(t.Context(), store, openPeriods{}, noEvents{}, input(tc.entries...))

			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if len(store.inserted) != 0 || len(store.entries) != 0 || len(store.added) != 0 {
				t.Fatalf("a rejected posting writes nothing: %d transactions, deltas %v", len(store.inserted), store.added)
			}
		})
	}
	t.Run("sync account without a balance row", func(t *testing.T) {
		noBalance := account(t, domain.KindLiability, domain.BalanceModeSync)
		store := newFakeStore(cash, deposit)
		store.accounts[noBalance.ID] = noBalance // no materialized balance

		if _, err := Post(t.Context(), store, openPeriods{}, noEvents{}, input(entry(cash, 100), entry(noBalance, -100))); err == nil || len(store.inserted) != 0 {
			t.Fatalf("it must fail without writing: error = %v, transactions %d", err, len(store.inserted))
		}
	})
}

// TestPostReturnsTheOriginalOfARepeatedRef: with an already posted reference, Post returns the original
// with ErrDuplicateExternalRef and does not write, even if it would no longer fit (FR-11). If it was
// already there, it does not even lock balances. If another producer posts it while the locks are awaited
// (and with that it no longer fits) or between the locks and the INSERT, Post finds it and does not write
// either.
func TestPostReturnsTheOriginalOfARepeatedRef(t *testing.T) {
	cash, deposit := account(t, domain.KindAsset, domain.BalanceModeSync), account(t, domain.KindLiability, domain.BalanceModeSync)
	in := input(entry(deposit, 100), entry(cash, -100)) // no funds: it must not matter
	original := Transaction{TransactionRecord: TransactionRecord{
		ID: ids.New(), LegalEntityID: in.LegalEntityID, Source: in.Source, ExternalRef: in.ExternalRef,
	}}
	cases := map[string]struct {
		prepare    func(*fakeStore)
		wantLocked bool
	}{
		"already posted":          {func(s *fakeStore) { s.posted[refOf(original.TransactionRecord)] = original }, false},
		"posted during the locks": {func(s *fakeStore) { s.whileLocking = &original }, true},
		"posted during the INSERT": {func(s *fakeStore) { // with funds: the check passes and the INSERT finds the duplicate
			s.whileInserting = &original
			s.balances[cash.ID], s.balances[deposit.ID] = LockedBalance{Posted: 100}, LockedBalance{Posted: -100}
		}, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := newFakeStore(cash, deposit)
			tc.prepare(store)

			got, err := Post(t.Context(), store, openPeriods{}, noEvents{}, in)

			if !errors.Is(err, ErrDuplicateExternalRef) || got.ID != original.ID {
				t.Fatalf("= %s, %v; want the original %s with ErrDuplicateExternalRef", got.ID, err, original.ID)
			}
			if len(store.inserted) != 0 || len(store.added) != 0 || (len(store.locked) > 0) != tc.wantLocked {
				t.Fatalf("a duplicate does not write: %d transactions, deltas %v, locked %v", len(store.inserted), store.added, store.locked)
			}
		})
	}
}

func TestPostDoesNotTakeAnotherEntitysRefAsRepeated(t *testing.T) {
	cash, deposit := account(t, domain.KindAsset, domain.BalanceModeSync), account(t, domain.KindLiability, domain.BalanceModeSync)
	in := input(entry(cash, 100), entry(deposit, -100))
	store := newFakeStore(cash, deposit)
	store.posted[ref{ids.New(), in.Source, in.ExternalRef}] = Transaction{TransactionRecord: TransactionRecord{ID: ids.New()}}

	if _, err := Post(t.Context(), store, openPeriods{}, noEvents{}, in); err != nil || len(store.inserted) != 1 {
		t.Fatalf("the same reference in another entity = %v, %d inserted; want it posted", err, len(store.inserted))
	}
}

func TestPostValidatesTheHeader(t *testing.T) {
	cash, deposit := account(t, domain.KindAsset, domain.BalanceModeSync), account(t, domain.KindLiability, domain.BalanceModeSync)
	cases := map[string]func(*PostInput){
		"no code":          func(in *PostInput) { in.Code = "" },
		"no channel":       func(in *PostInput) { in.Channel = "" },
		"no business date": func(in *PostInput) { in.BusinessDate = time.Time{} },
		"no source":        func(in *PostInput) { in.Source = "" },
		"no external_ref":  func(in *PostInput) { in.ExternalRef = "" },
		"no actor":         func(in *PostInput) { in.Actor = "" },
		"no rule_version":  func(in *PostInput) { in.RuleVersion = "" },
		"no value date":    func(in *PostInput) { in.ValueDate = time.Time{} },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			store := newFakeStore(cash, deposit)
			in := input(entry(cash, 1), entry(deposit, -1))
			breakIt(&in)

			if _, err := Post(t.Context(), store, openPeriods{}, noEvents{}, in); !errors.Is(err, ErrInvalidPosting) || len(store.inserted) != 0 {
				t.Fatalf("error = %v, want ErrInvalidPosting without writing", err)
			}
		})
	}
}

// TestPostKeepsTheCalendarDay: a date written in another time zone is stored with its same day.
func TestPostKeepsTheCalendarDay(t *testing.T) {
	cash, deposit := account(t, domain.KindAsset, domain.BalanceModeSync), account(t, domain.KindLiability, domain.BalanceModeSync)
	store := newFakeStore(cash, deposit)
	bogota := time.FixedZone("COT", -5*60*60)
	in := input(entry(cash, 1), entry(deposit, -1))
	in.ValueDate = time.Date(2026, 10, 26, 23, 30, 0, 0, bogota) // in UTC it is already the 27th
	in.BusinessDate = in.ValueDate

	posted, err := Post(t.Context(), store, openPeriods{}, noEvents{}, in)
	if err != nil {
		t.Fatal(err)
	}

	want := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	rec := store.inserted[0]
	if !posted.ValueDate.Equal(want) || !rec.ValueDate.Equal(want) || !rec.BusinessDate.Equal(want) {
		t.Fatalf("stored dates %v and %v, want %v", rec.ValueDate, rec.BusinessDate, want)
	}
}

func TestCreateAccountValidatesItsInput(t *testing.T) {
	valid := CreateAccountInput{LegalEntityID: entity, Code: "cash", Kind: domain.KindAsset, Currency: cop(t), Mode: domain.BalanceModeSync}
	cases := map[string]func(*CreateAccountInput){
		"no code":      func(in *CreateAccountInput) { in.Code = "" },
		"no currency":  func(in *CreateAccountInput) { in.Currency = money.Currency{} },
		"unknown kind": func(in *CreateAccountInput) { in.Kind = "revenue" },
		"unknown mode": func(in *CreateAccountInput) { in.Mode = "lazy" },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			in := valid
			breakIt(&in)

			if _, err := CreateAccount(t.Context(), newFakeStore(), in); !errors.Is(err, ErrInvalidAccount) {
				t.Fatalf("error = %v, want ErrInvalidAccount", err)
			}
		})
	}
	store := newFakeStore()
	a, err := CreateAccount(t.Context(), store, valid)
	if err != nil || store.accounts[a.ID] != a {
		t.Fatalf("the valid account must be registered: %v", err)
	}
}
