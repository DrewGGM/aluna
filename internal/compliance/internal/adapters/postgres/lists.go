package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/compliance/internal/app"
	"github.com/DrewGGM/aluna/internal/compliance/internal/domain/matching"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// Lists is app.ListStore over compliance.lists and compliance.list_entries: each change in its own transaction of
// uow, so that a refresh does not hold one across its downloads.
type Lists struct{ uow *postgres.UnitOfWork }

// NewLists is the store of the lists within uow.
func NewLists(uow *postgres.UnitOfWork) Lists { return Lists{uow: uow} }

// Stored is the version of list as last stored and how many entries it has: "" and 0 if it never was.
func (l Lists) Stored(ctx context.Context, list string) (string, int, error) {
	var (
		version string
		entries int
	)
	err := l.uow.Do(ctx, func(tx postgres.Tx) error {
		err := tx.QueryRow(ctx, `SELECT version, entries FROM compliance.lists WHERE list = $1`, list).Scan(&version, &entries)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return "", 0, fmt.Errorf("stored list %s: %w", list, err)
	}
	return version, entries, nil
}

// Replace stores entries as list's, of version, refreshed now, in place of what it had, in one transaction.
func (l Lists) Replace(ctx context.Context, list, version string, entries []matching.Entry) error {
	err := l.uow.Do(ctx, func(tx postgres.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO compliance.lists (list, version, entries) VALUES ($1, $2, $3)
			ON CONFLICT (list) DO UPDATE SET version = $2, entries = $3, refreshed_at = clock_timestamp()`, list, version, len(entries)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM compliance.list_entries WHERE list = $1`, list); err != nil {
			return err
		}
		_, err := tx.Pgx().CopyFrom(ctx, pgx.Identifier{"compliance", "list_entries"}, []string{"list", "entry_id", "names", "documents"},
			pgx.CopyFromSlice(len(entries), func(i int) ([]any, error) {
				e := entries[i]
				documents := e.Documents
				if documents == nil {
					documents = []string{}
				}
				return []any{list, e.ID, e.Names, documents}, nil
			}))
		return err
	})
	if err != nil {
		return fmt.Errorf("replace list %s: %w", list, err)
	}
	return nil
}

// Touch records that list, still of the same version, was refreshed now.
func (l Lists) Touch(ctx context.Context, list string) error {
	err := l.uow.Do(ctx, func(tx postgres.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE compliance.lists SET refreshed_at = clock_timestamp() WHERE list = $1`, list)
		return err
	})
	if err != nil {
		return fmt.Errorf("touch list %s: %w", list, err)
	}
	return nil
}

// Watchlist is app.Watchlist over q: the lists as last loaded, their entries indexed in memory. Screening runs
// within the transaction of the operation it screens, so it never reads the lists itself, which would take a
// second connection of the pool: Load reads them, at the api's start and then periodically (Poll), reading the
// entries again only when a version changed. An index is kept with the refresh times of its own lists, so that
// one is never taken as newer than it is.
type Watchlist struct {
	q       postgres.Querier
	mu      sync.RWMutex
	key     string
	current app.LoadedLists
	loaded  bool
}

// errNotLoaded means a watchlist that was never loaded: screening fails closed.
var errNotLoaded = fmt.Errorf("%w: the lists were never loaded", app.ErrStaleLists)

// NewWatchlist is the watchlist over q, the pool, not loaded yet.
func NewWatchlist(q postgres.Querier) *Watchlist { return &Watchlist{q: q} }

// Current is the lists as last loaded; errNotLoaded before the first Load.
func (w *Watchlist) Current() (app.LoadedLists, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if !w.loaded {
		return app.LoadedLists{}, errNotLoaded
	}
	return w.current, nil
}

// Poll loads the lists every every until ctx ends. A load that fails is logged to log and keeps the lists it had,
// which screening refuses once they are too old.
func (w *Watchlist) Poll(ctx context.Context, every time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.Load(ctx); err != nil && ctx.Err() == nil {
				log.ErrorContext(ctx, "reload the restrictive lists", "error", err)
			}
		}
	}
}

// Load reads the lists' versions and refresh times and, if a version changed, their entries, and indexes them.
func (w *Watchlist) Load(ctx context.Context) error {
	key, refreshed, err := w.versions(ctx)
	if err != nil {
		return err
	}
	w.mu.RLock()
	same, index := w.loaded && w.key == key, w.current.Index
	w.mu.RUnlock()
	if !same {
		if index, err = w.read(ctx); err != nil {
			return err
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.key, w.current, w.loaded = key, app.LoadedLists{Index: index, Refreshed: refreshed}, true
	return nil
}

// versions are the lists' versions, as one key, and when each was last refreshed.
func (w *Watchlist) versions(ctx context.Context) (string, map[string]time.Time, error) {
	rows, err := w.q.Query(ctx, `SELECT list, version, refreshed_at FROM compliance.lists ORDER BY list`)
	if err != nil {
		return "", nil, fmt.Errorf("read the lists: %w", err)
	}
	refreshed, versions := map[string]time.Time{}, []string{}
	var (
		list, version string
		at            time.Time
	)
	if _, err := pgx.ForEachRow(rows, []any{&list, &version, &at}, func() error {
		refreshed[list] = at
		versions = append(versions, list+"="+version)
		return nil
	}); err != nil {
		return "", nil, fmt.Errorf("read the lists: %w", err)
	}
	return strings.Join(versions, ","), refreshed, nil
}

// read is the index of the entries of the lists.
func (w *Watchlist) read(ctx context.Context) (*matching.Index, error) {
	rows, err := w.q.Query(ctx, `SELECT list, entry_id, names, documents FROM compliance.list_entries`)
	if err != nil {
		return nil, fmt.Errorf("read the list entries: %w", err)
	}
	entries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (matching.Entry, error) {
		var e matching.Entry
		err := row.Scan(&e.List, &e.ID, &e.Names, &e.Documents)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the list entries: %w", err)
	}
	return matching.NewIndex(entries), nil
}
