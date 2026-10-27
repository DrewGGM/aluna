// Package postgres es el adapter del ledger sobre PostgreSQL: implementa los puertos de app con el
// código que genera sqlc desde queries.sql. Cada consulta traduce sus propios errores con significado
// de dominio y envuelve los demás con %w y el nombre de la operación: 40001 y 40P01 siguen siendo
// reconocibles para que UnitOfWork.Do reintente (ADR-0009).
package postgres

import (
	"context"
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
		cur, err := money.CurrencyOf(r.Currency)
		if err != nil {
			return nil, fmt.Errorf("cuenta %s: %w", r.Code, err)
		}
		accounts[r.ID] = domain.Account{
			ID: r.ID, LegalEntityID: r.LegalEntityID, Code: r.Code, Kind: domain.Kind(r.Kind),
			Currency: cur, Mode: domain.BalanceMode(r.BalanceMode), LowerLimit: r.LowerLimit,
		}
	}
	return accounts, nil
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

// InsertTransaction inserta la cabecera y devuelve el posted_at que fijó la base.
func (s *Store) InsertTransaction(ctx context.Context, rec app.TransactionRecord) (time.Time, error) {
	postedAt, err := s.q.InsertTransaction(ctx, sqlcgen.InsertTransactionParams{
		ID: rec.ID, LegalEntityID: rec.LegalEntityID, Code: rec.Code, ValueDate: rec.ValueDate,
		BusinessDate: rec.BusinessDate, Source: rec.Source, ExternalRef: rec.ExternalRef, Actor: rec.Actor,
		Channel: rec.Channel, RuleVersion: rec.RuleVersion,
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("insertar la transacción: %w", err)
	}
	return postedAt, nil
}

// InsertEntries inserta los entries de la transacción txID en una sola sentencia.
func (s *Store) InsertEntries(ctx context.Context, txID ids.ID, entries []domain.EntryInput) error {
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

// AddToBalance suma delta al saldo de la cuenta y aumenta su versión.
func (s *Store) AddToBalance(ctx context.Context, accountID ids.ID, delta int64) error {
	if err := s.q.AddToBalance(ctx, sqlcgen.AddToBalanceParams{Delta: delta, AccountID: accountID}); err != nil {
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
