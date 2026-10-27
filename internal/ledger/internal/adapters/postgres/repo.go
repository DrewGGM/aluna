// Package postgres es el adapter del ledger sobre PostgreSQL: implementa los puertos de app con el
// código que genera sqlc desde queries.sql. Cada consulta traduce sus propios errores con significado
// de dominio y envuelve los demás con %w y el nombre de la operación: 40001 y 40P01 siguen siendo
// reconocibles para que UnitOfWork.Do reintente (ADR-0009).
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger/internal/adapters/postgres/sqlcgen"
	"github.com/DrewGGM/aluna/internal/ledger/internal/app"
	"github.com/DrewGGM/aluna/internal/ledger/internal/domain"
	"github.com/DrewGGM/aluna/internal/money"
)

// Store implementa los puertos de app sobre una conexión o transacción de base.
type Store struct{ q *sqlcgen.Queries }

var (
	_ app.PostingStore  = (*Store)(nil)
	_ app.AccountWriter = (*Store)(nil)
)

// NewStore crea el Store sobre db: una postgres.Tx dentro de UnitOfWork.Do o el pool para lecturas.
func NewStore(db sqlcgen.DBTX) *Store { return &Store{q: sqlcgen.New(db)} }

// InsertAccount registra la cuenta y su fila de saldo en cero (también las async: el agregador de
// T-023 la actualiza).
func (s *Store) InsertAccount(ctx context.Context, a domain.Account) error {
	err := s.q.InsertAccount(ctx, sqlcgen.InsertAccountParams{
		ID: a.ID, LegalEntityID: a.LegalEntityID, Code: a.Code, Kind: string(a.Kind),
		Currency: a.Currency.Code(), BalanceMode: string(a.Mode), LowerLimit: a.LowerLimit,
	})
	if isUniqueViolation(err, "accounts_legal_entity_id_code_key") {
		return fmt.Errorf("%w: el código %q ya existe en la entidad", app.ErrInvalidAccount, a.Code)
	}
	if err != nil {
		return fmt.Errorf("insertar la cuenta: %w", err)
	}
	if err := s.q.InsertBalance(ctx, a.ID); err != nil {
		return fmt.Errorf("crear el saldo de la cuenta: %w", err)
	}
	return nil
}

// Accounts devuelve las cuentas que existen entre accountIDs.
func (s *Store) Accounts(ctx context.Context, accountIDs []ids.ID) (map[ids.ID]domain.Account, error) {
	rows, err := s.q.AccountsByID(ctx, accountIDs)
	if err != nil {
		return nil, fmt.Errorf("leer las cuentas: %w", err)
	}
	accounts := make(map[ids.ID]domain.Account, len(rows))
	for _, r := range rows {
		a, err := toAccount(r.LedgerAccount)
		if err != nil {
			return nil, err
		}
		accounts[a.ID] = a
	}
	return accounts, nil
}

// AccountOf lee una cuenta de la entidad; una cuenta de otra entidad es ErrAccountNotFound.
func (s *Store) AccountOf(ctx context.Context, entity, id ids.ID) (domain.Account, error) {
	r, err := s.q.AccountOfEntity(ctx, sqlcgen.AccountOfEntityParams{ID: id, LegalEntityID: entity})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Account{}, fmt.Errorf("%w: %s", app.ErrAccountNotFound, id)
	}
	if err != nil {
		return domain.Account{}, fmt.Errorf("leer la cuenta: %w", err)
	}
	return toAccount(r.LedgerAccount)
}

func toAccount(r sqlcgen.LedgerAccount) (domain.Account, error) {
	cur, err := money.CurrencyOf(r.Currency)
	if err != nil {
		return domain.Account{}, fmt.Errorf("cuenta %s: %w", r.Code, err)
	}
	return domain.Account{
		ID: r.ID, LegalEntityID: r.LegalEntityID, Code: r.Code, Kind: domain.Kind(r.Kind),
		Currency: cur, Mode: domain.BalanceMode(r.BalanceMode), LowerLimit: r.LowerLimit,
	}, nil
}

// TransactionOf lee una transacción de la entidad con sus entries, en el orden en que se asentaron.
func (s *Store) TransactionOf(ctx context.Context, entity, id ids.ID) (app.Transaction, error) {
	r, err := s.q.TransactionOfEntity(ctx, sqlcgen.TransactionOfEntityParams{ID: id, LegalEntityID: entity})
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Transaction{}, fmt.Errorf("%w: %s", app.ErrTransactionNotFound, id)
	}
	if err != nil {
		return app.Transaction{}, fmt.Errorf("leer la transacción: %w", err)
	}
	rows, err := s.q.EntriesOf(ctx, id)
	if err != nil {
		return app.Transaction{}, fmt.Errorf("leer los entries: %w", err)
	}
	entries := make([]domain.EntryInput, len(rows))
	for i, e := range rows {
		cur, err := money.CurrencyOf(e.Currency)
		if err != nil {
			return app.Transaction{}, fmt.Errorf("entry de %s: %w", id, err)
		}
		entries[i] = domain.EntryInput{AccountID: e.AccountID, Amount: money.New(e.Amount, cur)}
	}
	rec, err := recordOfRow(r)
	if err != nil {
		return app.Transaction{}, err
	}
	return app.Transaction{TransactionRecord: rec, PostedAt: r.PostedAt, Entries: entries}, nil
}

func recordOfRow(r sqlcgen.TransactionOfEntityRow) (app.TransactionRecord, error) {
	var metadata map[string]string
	if err := json.Unmarshal(r.Metadata, &metadata); err != nil {
		return app.TransactionRecord{}, fmt.Errorf("metadata de %s: %w", r.ID, err)
	}
	if len(metadata) == 0 {
		metadata = nil // como la escribió Post: lo leído es igual a lo que Post devolvió
	}
	rec := app.TransactionRecord{
		ID: r.ID, LegalEntityID: r.LegalEntityID, Code: r.Code, ValueDate: r.ValueDate, BusinessDate: r.BusinessDate,
		Source: r.Source, ExternalRef: r.ExternalRef, Actor: r.Actor, Channel: r.Channel, RuleVersion: r.RuleVersion,
		Metadata: metadata,
	}
	if r.ReversesID != nil {
		rec.ReversesID = *r.ReversesID
	}
	return rec, nil
}

// LockBalances bloquea (FOR UPDATE, en orden de account_id) y devuelve los saldos de accountIDs.
func (s *Store) LockBalances(ctx context.Context, accountIDs []ids.ID) (map[ids.ID]app.LockedBalance, error) {
	rows, err := s.q.LockBalances(ctx, accountIDs)
	if err != nil {
		return nil, fmt.Errorf("bloquear los saldos: %w", err)
	}
	balances := make(map[ids.ID]app.LockedBalance, len(rows))
	for _, r := range rows {
		balances[r.AccountID] = app.LockedBalance{Posted: r.Posted, PendingDebits: r.PendingDebits, PendingCredits: r.PendingCredits}
	}
	return balances, nil
}

// TransactionByRef lee la transacción de la entidad con (source, ref), con sus entries; found dice si
// la hay.
func (s *Store) TransactionByRef(ctx context.Context, entity ids.ID, source, ref string) (app.Transaction, bool, error) {
	id, err := s.q.TransactionIDByRef(ctx, sqlcgen.TransactionIDByRefParams{LegalEntityID: entity, Source: source, ExternalRef: ref})
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Transaction{}, false, nil
	}
	if err != nil {
		return app.Transaction{}, false, fmt.Errorf("buscar la transacción por referencia: %w", err)
	}
	tx, err := s.TransactionOf(ctx, entity, id)
	return tx, err == nil, err
}

// InsertTransaction inserta la cabecera y sus entries y devuelve el posted_at que fijó la base. Una
// referencia repetida no se inserta: así la transacción de base sigue usable para releer la original,
// sin el 23505 que la abortaría. El ON CONFLICT no nombra columnas para cubrir también
// transactions_single_reversal: dos reversas concurrentes de la misma transacción chocan en los dos
// índices, y sin locks de saldo (cuentas async) la segunda vería el 23505 de ese. Si otra transacción
// insertó la misma fila y no ha terminado, el INSERT la espera y decide según su commit o su rollback.
func (s *Store) InsertTransaction(ctx context.Context, rec app.TransactionRecord, entries []domain.EntryInput) (time.Time, error) {
	metadata := []byte("{}") // json.Marshal de un mapa nil da null, y la columna espera un objeto
	if len(rec.Metadata) > 0 {
		var err error
		if metadata, err = json.Marshal(rec.Metadata); err != nil {
			return time.Time{}, fmt.Errorf("metadata de la transacción: %w", err)
		}
	}
	var reverses *ids.ID
	if rec.ReversesID != (ids.ID{}) {
		reverses = &rec.ReversesID
	}
	postedAt, err := s.q.InsertTransaction(ctx, sqlcgen.InsertTransactionParams{
		ID: rec.ID, LegalEntityID: rec.LegalEntityID, Code: rec.Code, ValueDate: rec.ValueDate,
		BusinessDate: rec.BusinessDate, Source: rec.Source, ExternalRef: rec.ExternalRef, Actor: rec.Actor,
		Channel: rec.Channel, RuleVersion: rec.RuleVersion, ReversesID: reverses, Metadata: metadata,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, fmt.Errorf("%w: %s/%s", app.ErrDuplicateExternalRef, rec.Source, rec.ExternalRef)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("insertar la transacción: %w", err)
	}
	if err := s.insertEntries(ctx, rec.ID, entries); err != nil {
		return time.Time{}, err
	}
	return postedAt, nil
}

// insertEntries inserta los entries de la transacción txID en una sola sentencia.
func (s *Store) insertEntries(ctx context.Context, txID ids.ID, entries []domain.EntryInput) error {
	params := sqlcgen.InsertEntriesParams{
		TransactionID: txID,
		AccountIds:    make([]ids.ID, len(entries)),
		Currencies:    make([]string, len(entries)),
		Amounts:       make([]int64, len(entries)),
	}
	for i, e := range entries {
		params.AccountIds[i], params.Currencies[i], params.Amounts[i] = e.AccountID, e.Amount.Currency().Code(), e.Amount.Units()
	}
	if err := s.q.InsertEntries(ctx, params); err != nil {
		return fmt.Errorf("insertar los entries: %w", err)
	}
	return nil
}

// AddToBalance suma d al saldo de la cuenta y aumenta su versión.
func (s *Store) AddToBalance(ctx context.Context, accountID ids.ID, d app.BalanceDelta) error {
	err := s.q.AddToBalance(ctx, sqlcgen.AddToBalanceParams{
		Posted: d.Posted, PendingDebits: d.PendingDebits, PendingCredits: d.PendingCredits, AccountID: accountID,
	})
	if err != nil {
		return fmt.Errorf("actualizar el saldo de %s: %w", accountID, err)
	}
	return nil
}

// Balance devuelve el saldo materializado de una cuenta, sin bloquearlo.
func (s *Store) Balance(ctx context.Context, accountID ids.ID) (app.Balance, error) {
	r, err := s.q.Balance(ctx, accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Balance{}, fmt.Errorf("%w: %s", app.ErrAccountNotFound, accountID)
	}
	if err != nil {
		return app.Balance{}, fmt.Errorf("leer el saldo: %w", err)
	}
	return app.Balance{Posted: r.Posted, PendingDebits: r.PendingDebits, PendingCredits: r.PendingCredits, Version: r.Version}, nil
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

// DatabaseNow es la hora de la base (clock_timestamp), la misma que fija posted_at.
func (s *Store) DatabaseNow(ctx context.Context) (time.Time, error) {
	now, err := s.q.DatabaseNow(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("leer la hora de la base: %w", err)
	}
	return now, nil
}

// InsertHold guarda un hold nuevo de la entidad.
func (s *Store) InsertHold(ctx context.Context, entity ids.ID, h app.Hold) error {
	err := s.q.InsertHold(ctx, sqlcgen.InsertHoldParams{
		ID: h.ID(), LegalEntityID: entity, AccountID: h.AccountID(), CounterAccountID: h.CounterAccountID(),
		Currency: h.Amount().Currency().Code(), Amount: h.Amount().Units(), ExpiresAt: h.ExpiresAt(),
	})
	if err != nil {
		return fmt.Errorf("insertar el hold: %w", err)
	}
	return nil
}

// LockHold bloquea el hold de la entidad (FOR UPDATE) y lo reconstruye.
func (s *Store) LockHold(ctx context.Context, entity, id ids.ID) (app.Hold, error) {
	r, err := s.q.LockHold(ctx, sqlcgen.LockHoldParams{ID: id, LegalEntityID: entity})
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Hold{}, fmt.Errorf("%w: %s", app.ErrHoldNotFound, id)
	}
	if err != nil {
		return app.Hold{}, fmt.Errorf("bloquear el hold: %w", err)
	}
	return restoredHold(r)
}

// LockDueHolds bloquea un lote de holds pending vencidos a now, saltando los bloqueados.
func (s *Store) LockDueHolds(ctx context.Context, now time.Time, limit int) ([]app.Hold, error) {
	rows, err := s.q.LockDueHolds(ctx, sqlcgen.LockDueHoldsParams{Now: now, Batch: int64(limit)})
	if err != nil {
		return nil, fmt.Errorf("bloquear los holds vencidos: %w", err)
	}
	due := make([]app.Hold, len(rows))
	for i, r := range rows {
		if due[i], err = restoredHold(sqlcgen.LockHoldRow(r)); err != nil {
			return nil, err
		}
	}
	return due, nil
}

// restoredHold reconstruye un hold leído. Una fila que no se puede reconstruir es un dato corrupto, no
// un error del cliente: se informa sin el error de dominio para que llegue como 500 y no se guarde como
// respuesta de la clave.
func restoredHold(r sqlcgen.LockHoldRow) (app.Hold, error) {
	cur, err := money.CurrencyOf(r.Currency)
	if err != nil {
		return app.Hold{}, fmt.Errorf("hold %s corrupto: %v", r.ID, err) //nolint:errorlint // ver restoredHold
	}
	h, err := domain.RestoreHold(domain.HoldSnapshot{
		ID: r.ID, AccountID: r.AccountID, Amount: money.New(r.Amount, cur), Captured: money.New(r.CapturedAmount, cur),
		Status: domain.HoldStatus(r.Status), ExpiresAt: r.ExpiresAt,
	})
	if err != nil {
		return app.Hold{}, fmt.Errorf("hold %s corrupto: %v", r.ID, err) //nolint:errorlint // ver restoredHold
	}
	return app.RestoredHold(h, r.CounterAccountID), nil
}

// CloseHold guarda el resultado de un hold que salió de pending.
func (s *Store) CloseHold(ctx context.Context, h app.Hold, captureTxID ids.ID) error {
	var capture *ids.ID
	if captureTxID != (ids.ID{}) {
		capture = &captureTxID
	}
	err := s.q.CloseHold(ctx, sqlcgen.CloseHoldParams{
		Status: string(h.Status()), CapturedAmount: h.Captured().Units(), CaptureTxID: capture, ID: h.ID(),
	})
	if err != nil {
		return fmt.Errorf("cerrar el hold %s: %w", h.ID(), err)
	}
	return nil
}

// LockAsyncCursor bloquea y lee el cursor del agregador.
func (s *Store) LockAsyncCursor(ctx context.Context) (app.AsyncCursor, error) {
	r, err := s.q.LockAsyncCursor(ctx)
	if err != nil {
		return app.AsyncCursor{}, fmt.Errorf("bloquear el cursor de agregación: %w", err)
	}
	return app.AsyncCursor{Xact: r.PostedXact, EntryID: r.EntryID}, nil
}

// AsyncEntriesAfter lee un lote de entries de cuentas async posteriores a c y el cursor del último.
func (s *Store) AsyncEntriesAfter(ctx context.Context, c app.AsyncCursor, limit int) ([]domain.EntryInput, app.AsyncCursor, error) {
	rows, err := s.q.AsyncEntriesAfter(ctx, sqlcgen.AsyncEntriesAfterParams{PostedXact: c.Xact, EntryID: c.EntryID, Batch: int64(limit)})
	if err != nil {
		return nil, c, fmt.Errorf("leer los entries async: %w", err)
	}
	entries := make([]domain.EntryInput, len(rows))
	for i, r := range rows {
		cur, err := money.CurrencyOf(r.Currency)
		if err != nil {
			return nil, c, fmt.Errorf("entry %d: %w", r.ID, err)
		}
		entries[i] = domain.EntryInput{AccountID: r.AccountID, Amount: money.New(r.Amount, cur)}
		c = app.AsyncCursor{Xact: r.PostedXact, EntryID: r.ID}
	}
	return entries, c, nil
}

// MoveAsyncCursor deja el cursor en c.
func (s *Store) MoveAsyncCursor(ctx context.Context, c app.AsyncCursor) error {
	if err := s.q.MoveAsyncCursor(ctx, sqlcgen.MoveAsyncCursorParams{PostedXact: c.Xact, EntryID: c.EntryID}); err != nil {
		return fmt.Errorf("mover el cursor de agregación: %w", err)
	}
	return nil
}
