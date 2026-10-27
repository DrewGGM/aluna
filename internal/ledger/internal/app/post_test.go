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

// ref es la clave de la unicidad de FR-11.
type ref struct {
	entity         ids.ID
	source, extRef string
}

// fakeStore es un PostingStore y AccountWriter en memoria que registra qué se bloqueó y qué se escribió.
// posted son las transacciones ya asentadas. Para simular a otro productor con la misma referencia,
// whileLocking aparece al bloquear los saldos y whileInserting al insertar.
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

	_, err := Post(t.Context(), store, input(entry(cash, 70), entry(cash, 30), entry(deposit, -100)))
	if err != nil {
		t.Fatal(err)
	}
	if store.added[cash.ID] != 100 || store.added[deposit.ID] != -100 {
		t.Errorf("deltas = %v, se esperaba +100 en caja y -100 en el depósito", store.added)
	}
	if len(store.locked) != 2 || len(store.entries) != 3 {
		t.Errorf("locks = %v, entries = %d: una vez por cuenta y los tres entries", store.locked, len(store.entries))
	}
}

// TestPostChecksTheNetNotEachEntry: un débito y un crédito iguales sobre una cuenta ya bajo su límite
// no la empeoran; revisar entry por entry la rechazaría.
func TestPostChecksTheNetNotEachEntry(t *testing.T) {
	cash, deposit := account(t, domain.KindAsset, domain.BalanceModeSync), account(t, domain.KindLiability, domain.BalanceModeSync)
	store := newFakeStore(cash, deposit)
	store.balances[deposit.ID] = LockedBalance{Posted: 50} // natural -50: bajo su límite de 0

	_, err := Post(t.Context(), store, input(entry(deposit, 10), entry(deposit, -10), entry(cash, 5), entry(cash, -5)))
	if err != nil {
		t.Fatalf("un neto de cero no reduce el saldo: %v", err)
	}
}

func TestPostSkipsAsyncAccounts(t *testing.T) {
	deposit, clearing := account(t, domain.KindLiability, domain.BalanceModeSync), account(t, domain.KindAsset, domain.BalanceModeAsync)
	store := newFakeStore(deposit, clearing)

	if _, err := Post(t.Context(), store, input(entry(clearing, 100), entry(deposit, -100))); err != nil {
		t.Fatal(err)
	}

	if slices.Contains(store.locked, clearing.ID) || store.added[clearing.ID] != 0 {
		t.Errorf("la cuenta async no se bloquea ni se actualiza aquí: locks %v, deltas %v", store.locked, store.added)
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
		"desbalanceado":          {[]domain.EntryInput{entry(cash, 100), entry(deposit, -99)}, domain.ErrUnbalanced},
		"cuenta de otra entidad": {[]domain.EntryInput{entry(cash, 100), entry(foreign, -100)}, ErrAccountNotFound},
		"cuenta inexistente":     {[]domain.EntryInput{entry(cash, 100), {AccountID: ids.New(), Amount: money.New(-100, cop(t))}}, ErrAccountNotFound},
		"moneda de otra cuenta":  {[]domain.EntryInput{{AccountID: cash.ID, Amount: money.New(100, usd)}, {AccountID: deposit.ID, Amount: money.New(-100, usd)}}, ErrCurrencyMismatch},
		"sobregiro":              {[]domain.EntryInput{entry(deposit, 100), entry(cash, -100)}, domain.ErrInsufficientFunds},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := newFakeStore(cash, deposit, foreign)

			_, err := Post(t.Context(), store, input(tc.entries...))

			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, se esperaba %v", err, tc.want)
			}
			if len(store.inserted) != 0 || len(store.entries) != 0 || len(store.added) != 0 {
				t.Fatalf("un posting rechazado no escribe nada: %d transacciones, deltas %v", len(store.inserted), store.added)
			}
		})
	}
	t.Run("cuenta sync sin fila de saldo", func(t *testing.T) {
		noBalance := account(t, domain.KindLiability, domain.BalanceModeSync)
		store := newFakeStore(cash, deposit)
		store.accounts[noBalance.ID] = noBalance // sin saldo materializado

		if _, err := Post(t.Context(), store, input(entry(cash, 100), entry(noBalance, -100))); err == nil || len(store.inserted) != 0 {
			t.Fatalf("debe fallar sin escribir: error = %v, transacciones %d", err, len(store.inserted))
		}
	})
}

// TestPostReturnsTheOriginalOfARepeatedRef: con una referencia ya asentada, Post devuelve la original
// con ErrDuplicateExternalRef y no escribe, también si ya no cabría (FR-11). Si ya estaba, ni bloquea
// saldos. Si otro productor la asienta mientras se esperan los locks (y con eso ya no alcanza) o entre
// los locks y el INSERT, Post la encuentra y tampoco escribe.
func TestPostReturnsTheOriginalOfARepeatedRef(t *testing.T) {
	cash, deposit := account(t, domain.KindAsset, domain.BalanceModeSync), account(t, domain.KindLiability, domain.BalanceModeSync)
	in := input(entry(deposit, 100), entry(cash, -100)) // sin fondos: no debe importar
	original := Transaction{TransactionRecord: TransactionRecord{
		ID: ids.New(), LegalEntityID: in.LegalEntityID, Source: in.Source, ExternalRef: in.ExternalRef,
	}}
	cases := map[string]struct {
		prepare    func(*fakeStore)
		wantLocked bool
	}{
		"ya asentada":           {func(s *fakeStore) { s.posted[refOf(original.TransactionRecord)] = original }, false},
		"asentada en los locks": {func(s *fakeStore) { s.whileLocking = &original }, true},
		"asentada en el INSERT": {func(s *fakeStore) { // con fondos: la validación pasa y el INSERT descubre el repetido
			s.whileInserting = &original
			s.balances[cash.ID], s.balances[deposit.ID] = LockedBalance{Posted: 100}, LockedBalance{Posted: -100}
		}, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := newFakeStore(cash, deposit)
			tc.prepare(store)

			got, err := Post(t.Context(), store, in)

			if !errors.Is(err, ErrDuplicateExternalRef) || got.ID != original.ID {
				t.Fatalf("= %s, %v; se esperaba la original %s con ErrDuplicateExternalRef", got.ID, err, original.ID)
			}
			if len(store.inserted) != 0 || len(store.added) != 0 || (len(store.locked) > 0) != tc.wantLocked {
				t.Fatalf("un repetido no escribe: %d transacciones, deltas %v, bloqueó %v", len(store.inserted), store.added, store.locked)
			}
		})
	}
}

func TestPostDoesNotTakeAnotherEntitysRefAsRepeated(t *testing.T) {
	cash, deposit := account(t, domain.KindAsset, domain.BalanceModeSync), account(t, domain.KindLiability, domain.BalanceModeSync)
	in := input(entry(cash, 100), entry(deposit, -100))
	store := newFakeStore(cash, deposit)
	store.posted[ref{ids.New(), in.Source, in.ExternalRef}] = Transaction{TransactionRecord: TransactionRecord{ID: ids.New()}}

	if _, err := Post(t.Context(), store, in); err != nil || len(store.inserted) != 1 {
		t.Fatalf("la misma referencia en otra entidad = %v, %d insertadas; se esperaba asentarla", err, len(store.inserted))
	}
}

func TestPostValidatesTheHeader(t *testing.T) {
	cash, deposit := account(t, domain.KindAsset, domain.BalanceModeSync), account(t, domain.KindLiability, domain.BalanceModeSync)
	cases := map[string]func(*PostInput){
		"sin code":           func(in *PostInput) { in.Code = "" },
		"sin channel":        func(in *PostInput) { in.Channel = "" },
		"sin fecha contable": func(in *PostInput) { in.BusinessDate = time.Time{} },
		"sin source":         func(in *PostInput) { in.Source = "" },
		"sin external_ref":   func(in *PostInput) { in.ExternalRef = "" },
		"sin actor":          func(in *PostInput) { in.Actor = "" },
		"sin rule_version":   func(in *PostInput) { in.RuleVersion = "" },
		"sin fecha valor":    func(in *PostInput) { in.ValueDate = time.Time{} },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			store := newFakeStore(cash, deposit)
			in := input(entry(cash, 1), entry(deposit, -1))
			breakIt(&in)

			if _, err := Post(t.Context(), store, in); !errors.Is(err, ErrInvalidPosting) || len(store.inserted) != 0 {
				t.Fatalf("error = %v, se esperaba ErrInvalidPosting sin escribir", err)
			}
		})
	}
}

// TestPostKeepsTheCalendarDay: una fecha escrita en otra zona horaria se guarda con su mismo día.
func TestPostKeepsTheCalendarDay(t *testing.T) {
	cash, deposit := account(t, domain.KindAsset, domain.BalanceModeSync), account(t, domain.KindLiability, domain.BalanceModeSync)
	store := newFakeStore(cash, deposit)
	bogota := time.FixedZone("COT", -5*60*60)
	in := input(entry(cash, 1), entry(deposit, -1))
	in.ValueDate = time.Date(2026, 10, 26, 23, 30, 0, 0, bogota) // en UTC ya es el 27
	in.BusinessDate = in.ValueDate

	posted, err := Post(t.Context(), store, in)
	if err != nil {
		t.Fatal(err)
	}

	want := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	rec := store.inserted[0]
	if !posted.ValueDate.Equal(want) || !rec.ValueDate.Equal(want) || !rec.BusinessDate.Equal(want) {
		t.Fatalf("fechas guardadas %v y %v, se esperaba %v", rec.ValueDate, rec.BusinessDate, want)
	}
}

func TestCreateAccountValidatesItsInput(t *testing.T) {
	valid := CreateAccountInput{LegalEntityID: entity, Code: "caja", Kind: domain.KindAsset, Currency: cop(t), Mode: domain.BalanceModeSync}
	cases := map[string]func(*CreateAccountInput){
		"sin código":        func(in *CreateAccountInput) { in.Code = "" },
		"sin moneda":        func(in *CreateAccountInput) { in.Currency = money.Currency{} },
		"clase desconocida": func(in *CreateAccountInput) { in.Kind = "revenue" },
		"modo desconocido":  func(in *CreateAccountInput) { in.Mode = "lazy" },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			in := valid
			breakIt(&in)

			if _, err := CreateAccount(t.Context(), newFakeStore(), in); !errors.Is(err, ErrInvalidAccount) {
				t.Fatalf("error = %v, se esperaba ErrInvalidAccount", err)
			}
		})
	}
	store := newFakeStore()
	a, err := CreateAccount(t.Context(), store, valid)
	if err != nil || store.accounts[a.ID] != a {
		t.Fatalf("la cuenta válida debe registrarse: %v", err)
	}
}
