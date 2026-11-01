package app

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
)

// MaxPageSize is the most entries a page of history has.
const MaxPageSize = 200

// ErrInvalidPage means a page of history of no entries or of more than MaxPageSize, or a cursor the ledger did
// not issue.
var ErrInvalidPage = errors.New("invalid page")

// errUnknownCursor is a cursor the ledger did not issue.
var errUnknownCursor = fmt.Errorf("%w: unknown cursor", ErrInvalidPage)

// HistoryEntry is an entry of an account's history (FR-44): its amount, signed (debit +, credit −), with the
// transaction it belongs to. EntryID and PostedAt are its position in the history; the entry's ID is the
// ledger's own and not one to show.
type HistoryEntry struct {
	EntryID             int64
	TransactionID       ids.ID
	Code                string
	Amount              money.Amount
	ValueDate, PostedAt time.Time
}

// HistoryPage is a page of an account's history, newest first, and the cursor of the next one: empty on the
// last page.
type HistoryPage struct {
	Entries []HistoryEntry
	Next    string
}

// Position is a position in an account's history: the entries before it are those posted before PostedAt, or
// at PostedAt with an ID below EntryID. The zero Position is the newest one, that of the first page: every
// entry is before it.
type Position struct {
	PostedAt time.Time
	EntryID  int64
}

// HistoryReader is the persistence of History.
type HistoryReader interface {
	// EntriesBefore are the first limit entries of the entity's account before p, newest first by
	// (posted_at, id); another entity's account has none.
	EntriesBefore(ctx context.Context, entity, accountID ids.ID, p Position, limit int) ([]HistoryEntry, error)
}

// History is the page of limit entries of the history of the entity's account that follows cursor, or its
// first page when cursor is empty. Paging never repeats an entry nor skips one committed before the first
// page: the order of the committed entries does not change. One committed while paging shows up in a page
// not read yet or not at all, since its posted_at is when it was inserted and not when it was committed.
func History(ctx context.Context, r HistoryReader, entity, accountID ids.ID, cursor string, limit int) (HistoryPage, error) {
	if limit < 1 || limit > MaxPageSize {
		return HistoryPage{}, fmt.Errorf("%w: a page has 1 to %d entries, not %d", ErrInvalidPage, MaxPageSize, limit)
	}
	p, err := positionOf(cursor)
	if err != nil {
		return HistoryPage{}, err
	}
	entries, err := r.EntriesBefore(ctx, entity, accountID, p, limit+1)
	if err != nil {
		return HistoryPage{}, err
	}
	if len(entries) <= limit {
		return HistoryPage{Entries: entries}, nil
	}
	last := entries[limit-1]
	return HistoryPage{Entries: entries[:limit], Next: cursorOf(Position{PostedAt: last.PostedAt, EntryID: last.EntryID})}, nil
}

// cursorOf is p, opaque: base64 of the microseconds of its posted_at (the database's precision) and its entry
// ID, as varints.
func cursorOf(p Position) string {
	raw := binary.AppendVarint(binary.AppendVarint(nil, p.PostedAt.UnixMicro()), p.EntryID)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// positionOf is the position cursor marks, or the zero Position if it is empty. Only what cursorOf writes is a
// cursor: an entry posted after 1970 (posted_at comes from the database's clock) with an ID from 1, encoded
// as cursorOf encodes it. Any other time could fall outside PostgreSQL's range, and that error would not be
// the caller's ErrInvalidPage.
func positionOf(cursor string) (Position, error) {
	if cursor == "" {
		return Position{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return Position{}, errUnknownCursor
	}
	micros, n := binary.Varint(raw)
	if n <= 0 {
		return Position{}, errUnknownCursor
	}
	entryID, _ := binary.Varint(raw[n:]) // what is missing, overflows or is left over does not encode back the same
	p := Position{PostedAt: time.UnixMicro(micros).UTC(), EntryID: entryID}
	if micros < 0 || entryID < 1 || cursorOf(p) != cursor {
		return Position{}, errUnknownCursor
	}
	return p, nil
}
