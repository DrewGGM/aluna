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
	Accounts(ctx context.Context, accountIDs []ids.ID) (map[ids.ID]domain.Account, error)
	LockBalances(ctx context.Context, accountIDs []ids.ID) (map[ids.ID]LockedBalance, error)
	InsertTransaction(ctx context.Context, rec TransactionRecord) (time.Time, error)
	InsertEntries(ctx context.Context, txID ids.ID, entries []domain.EntryInput) error
	AddToBalance(ctx context.Context, accountID ids.ID, delta int64) error
}

// Post valida la cabecera y la partida doble, bloquea los saldos de las cuentas sync, revisa que el
// neto de cada cuenta quepa en ella y asienta transacción, entries y saldos.
func Post(ctx context.Context, store PostingStore, in PostInput) (Transaction, error) {
	rec, err := recordOf(in)
	if err != nil {
		return Transaction{}, err
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
	if err := checkAvailable(ctx, store, accounts, syncIDs, deltas); err != nil {
		return Transaction{}, err
	}
	postedAt, err := store.InsertTransaction(ctx, rec)
	if err != nil {
		return Transaction{}, err
	}
	if err := store.InsertEntries(ctx, tx.ID(), tx.Entries()); err != nil {
		return Transaction{}, err
	}
	for _, id := range syncIDs {
		if err := store.AddToBalance(ctx, id, deltas[id].Units()); err != nil {
			return Transaction{}, err
		}
	}
	return Transaction{TransactionRecord: rec, PostedAt: postedAt, Entries: tx.Entries()}, nil
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
