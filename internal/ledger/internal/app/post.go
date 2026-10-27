// Package app son los casos de uso del ledger. Cada caso de uso declara el puerto de persistencia que
// necesita y no conoce la base de datos: la fachada le entrega un adapter atado a la transacción de base
// en curso.
package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger/internal/domain"
	"github.com/DrewGGM/aluna/internal/money"
)

var (
	// ErrAccountNotFound indica una cuenta que no existe o que es de otra entidad legal.
	ErrAccountNotFound = errors.New("cuenta no encontrada")
	// ErrCurrencyMismatch indica un entry en una moneda distinta a la de su cuenta.
	ErrCurrencyMismatch = errors.New("el entry no está en la moneda de su cuenta")
	// ErrInvalidPosting indica una cabecera de posting incompleta.
	ErrInvalidPosting = errors.New("posting inválido")
	// ErrTransactionNotFound indica una transacción que no existe o que es de otra entidad legal.
	ErrTransactionNotFound = errors.New("transacción no encontrada")
	// ErrDuplicateExternalRef indica que la fuente ya asentó una transacción con esa referencia: Post la
	// devuelve junto con este error (FR-11).
	ErrDuplicateExternalRef = errors.New("la referencia externa ya se asentó")
)

// PostInput es un posting: su cabecera y sus entries. Las fechas valen por su día calendario tal como
// están escritas, sin importar la zona horaria del time.Time.
type PostInput struct {
	LegalEntityID           ids.ID
	Code                    string
	ValueDate, BusinessDate time.Time
	Source, ExternalRef     string
	Actor, Channel          string
	RuleVersion             string
	Entries                 []domain.EntryInput
}

// TransactionRecord es la cabecera que se persiste de una transacción.
type TransactionRecord struct {
	ID                      ids.ID
	LegalEntityID           ids.ID
	Code                    string
	ValueDate, BusinessDate time.Time // medianoche UTC del día calendario
	Source, ExternalRef     string
	Actor, Channel          string
	RuleVersion             string
}

// Transaction es una transacción asentada: su cabecera, el instante que fijó la base y sus entries.
type Transaction struct {
	TransactionRecord
	PostedAt time.Time
	Entries  []domain.EntryInput
}

// LockedBalance es el saldo bloqueado de una cuenta sync, con signo, contra el que se valida un posting.
type LockedBalance struct {
	Posted, PendingDebits, PendingCredits int64
}

// PostingStore es la persistencia de Post, toda dentro de una misma transacción de base.
type PostingStore interface {
	// TransactionByRef devuelve la transacción de la entidad con (source, ref); found dice si hay.
	TransactionByRef(ctx context.Context, entity ids.ID, source, ref string) (tx Transaction, found bool, err error)
	Accounts(ctx context.Context, accountIDs []ids.ID) (map[ids.ID]domain.Account, error)
	LockBalances(ctx context.Context, accountIDs []ids.ID) (map[ids.ID]LockedBalance, error)
	// InsertTransaction inserta cabecera y entries, y devuelve el posted_at que fijó la base. Si la
	// entidad ya tiene (source, external_ref), no inserta nada y devuelve ErrDuplicateExternalRef.
	InsertTransaction(ctx context.Context, rec TransactionRecord, entries []domain.EntryInput) (time.Time, error)
	AddToBalance(ctx context.Context, accountID ids.ID, delta int64) error
}

// Post valida la cabecera y la partida doble, bloquea los saldos de las cuentas sync, revisa que el
// neto de cada cuenta quepa en ella y asienta transacción, entries y saldos. Si la entidad ya asentó
// (source, external_ref) devuelve esa transacción original con ErrDuplicateExternalRef y no escribe
// nada (FR-11). El reintento de algo que ya pasó no debe fallar por un saldo que cambió después, así
// que la referencia se busca antes de validar, otra vez si los fondos no alcanzan (ver available) y al
// insertar, si otro productor la asentó sobre otras cuentas mientras tanto.
func Post(ctx context.Context, store PostingStore, in PostInput) (Transaction, error) {
	rec, err := recordOf(in)
	if err != nil {
		return Transaction{}, err
	}
	if original, found, err := originalOf(ctx, store, rec); found || err != nil {
		return original, err
	}
	tx, err := domain.NewTransaction(domain.NewTransactionInput{ID: rec.ID, Entries: in.Entries})
	if err != nil {
		return Transaction{}, err
	}
	accounts, err := accountsOf(ctx, store, rec.LegalEntityID, tx.Entries())
	if err != nil {
		return Transaction{}, err
	}
	deltas, err := netByAccount(tx.Entries())
	if err != nil {
		return Transaction{}, err
	}
	syncIDs := syncAccounts(accounts)
	if original, err := available(ctx, store, rec, accounts, syncIDs, deltas); err != nil {
		return original, err
	}
	return write(ctx, store, rec, tx.Entries(), syncIDs, deltas)
}

// available bloquea los saldos y revisa que el neto quepa. Si no cabe, busca otra vez la referencia:
// otro productor pudo asentarla sobre estas mismas cuentas mientras se esperaban sus locks, y con su
// commit el saldo ya no alcanza para repetirla; entonces el posting es la original, no un sobregiro.
// Cualquier otro error (un deadlock que aborta la transacción y UnitOfWork reintenta) sube tal cual.
func available(ctx context.Context, store PostingStore, rec TransactionRecord, accounts map[ids.ID]domain.Account,
	syncIDs []ids.ID, deltas map[ids.ID]money.Amount,
) (Transaction, error) {
	err := checkAvailable(ctx, store, accounts, syncIDs, deltas)
	if !errors.Is(err, domain.ErrInsufficientFunds) {
		return Transaction{}, err
	}
	if original, found, lookupErr := originalOf(ctx, store, rec); found || lookupErr != nil {
		return original, lookupErr
	}
	return Transaction{}, err
}

// write asienta la transacción ya validada y suma su neto a los saldos sync.
func write(ctx context.Context, store PostingStore, rec TransactionRecord, entries []domain.EntryInput, syncIDs []ids.ID,
	deltas map[ids.ID]money.Amount,
) (Transaction, error) {
	postedAt, err := store.InsertTransaction(ctx, rec, entries)
	if errors.Is(err, ErrDuplicateExternalRef) {
		original, found, lookupErr := originalOf(ctx, store, rec)
		if !found && lookupErr == nil { // el conflicto prueba que existe: sin verla, se falla cerrado y no como repetido
			lookupErr = fmt.Errorf("la referencia %s/%s chocó al insertar y no se ve la original", rec.Source, rec.ExternalRef)
		}
		return original, lookupErr
	}
	if err != nil {
		return Transaction{}, err
	}
	for _, id := range syncIDs {
		if err := store.AddToBalance(ctx, id, deltas[id].Units()); err != nil {
			return Transaction{}, err
		}
	}
	return Transaction{TransactionRecord: rec, PostedAt: postedAt, Entries: entries}, nil
}

// originalOf busca la transacción ya asentada con la referencia de rec. Si la hay (found), err es
// ErrDuplicateExternalRef, que Post devuelve junto con ella.
func originalOf(ctx context.Context, store PostingStore, rec TransactionRecord) (Transaction, bool, error) {
	original, found, err := store.TransactionByRef(ctx, rec.LegalEntityID, rec.Source, rec.ExternalRef)
	if err != nil || !found {
		return Transaction{}, false, err
	}
	return original, true, fmt.Errorf("%w: %s/%s es la transacción %s", ErrDuplicateExternalRef, rec.Source, rec.ExternalRef, original.ID)
}

// recordOf valida la cabecera y la lleva a la forma que se persiste.
func recordOf(in PostInput) (TransactionRecord, error) {
	required := []struct{ field, value string }{
		{"code", in.Code},
		{"source", in.Source},
		{"external_ref", in.ExternalRef},
		{"actor", in.Actor},
		{"channel", in.Channel},
		{"rule_version", in.RuleVersion},
	}
	for _, r := range required {
		if r.value == "" {
			return TransactionRecord{}, fmt.Errorf("%w: falta %s", ErrInvalidPosting, r.field)
		}
	}
	if in.ValueDate.IsZero() || in.BusinessDate.IsZero() {
		return TransactionRecord{}, fmt.Errorf("%w: faltan las fechas", ErrInvalidPosting)
	}
	return TransactionRecord{
		ID: ids.New(), LegalEntityID: in.LegalEntityID, Code: in.Code,
		ValueDate: civilDate(in.ValueDate), BusinessDate: civilDate(in.BusinessDate),
		Source: in.Source, ExternalRef: in.ExternalRef, Actor: in.Actor, Channel: in.Channel, RuleVersion: in.RuleVersion,
	}, nil
}

// civilDate conserva el día calendario de t como medianoche UTC: así la Transaction que devuelve Post
// es igual a la que se lee después de la base (date sin zona) y dos fechas del mismo día se comparan
// iguales aunque vengan escritas en zonas distintas.
func civilDate(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// accountsOf carga las cuentas de los entries y revisa que sean de la entidad y de la moneda del entry.
func accountsOf(ctx context.Context, store PostingStore, entity ids.ID, entries []domain.EntryInput) (map[ids.ID]domain.Account, error) {
	accountIDs := make([]ids.ID, 0, len(entries))
	for _, e := range entries {
		accountIDs = append(accountIDs, e.AccountID)
	}
	accounts, err := store.Accounts(ctx, accountIDs)
	if err != nil {
		return nil, err
	}
	for i, e := range entries {
		a, ok := accounts[e.AccountID]
		if !ok || a.LegalEntityID != entity {
			return nil, fmt.Errorf("%w: entry %d, cuenta %s", ErrAccountNotFound, i, e.AccountID)
		}
		if a.Currency != e.Amount.Currency() {
			return nil, fmt.Errorf("%w: entry %d en %s, cuenta %s en %s",
				ErrCurrencyMismatch, i, e.Amount.Currency().Code(), a.Code, a.Currency.Code())
		}
	}
	return accounts, nil
}

// netByAccount suma los entries de cada cuenta: es el movimiento neto que recibe su saldo.
func netByAccount(entries []domain.EntryInput) (map[ids.ID]money.Amount, error) {
	deltas := map[ids.ID]money.Amount{}
	for _, e := range entries {
		sum, ok := deltas[e.AccountID]
		if !ok {
			sum = money.New(0, e.Amount.Currency())
		}
		next, err := sum.Add(e.Amount)
		if err != nil {
			return nil, fmt.Errorf("cuenta %s: %w", e.AccountID, err)
		}
		deltas[e.AccountID] = next
	}
	return deltas, nil
}

// syncAccounts devuelve las cuentas sync. Las async no se bloquean ni se validan; su saldo lo agrega
// otro proceso (T-023). El orden de los locks lo pone la consulta (ORDER BY account_id).
func syncAccounts(accounts map[ids.ID]domain.Account) []ids.ID {
	var syncIDs []ids.ID
	for id, a := range accounts {
		if a.Mode == domain.BalanceModeSync {
			syncIDs = append(syncIDs, id)
		}
	}
	return syncIDs
}

func checkAvailable(ctx context.Context, store PostingStore, accounts map[ids.ID]domain.Account, syncIDs []ids.ID, deltas map[ids.ID]money.Amount) error {
	balances, err := store.LockBalances(ctx, syncIDs)
	if err != nil {
		return err
	}
	for _, id := range syncIDs {
		b, ok := balances[id]
		if !ok { // CreateAccount la crea siempre; si faltara, se falla en vez de validar contra cero
			return fmt.Errorf("cuenta %s sin saldo materializado", id)
		}
		if err := accounts[id].CheckAvailable(b.Posted, b.PendingDebits, b.PendingCredits, deltas[id].Units()); err != nil {
			return err
		}
	}
	return nil
}
