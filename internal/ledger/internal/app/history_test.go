package app

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
)

// fakeHistory is a history of n entries, newest first, one per microsecond; it records the position and limit it
// was asked for.
type fakeHistory struct {
	n     int
	asked Position
	limit int
}

func (h *fakeHistory) EntriesBefore(_ context.Context, _, _ ids.ID, p Position, limit int) ([]HistoryEntry, error) {
	h.asked, h.limit = p, limit
	var page []HistoryEntry
	for i := h.n; i > 0 && len(page) < limit; i-- {
		e := HistoryEntry{EntryID: int64(i), PostedAt: time.UnixMicro(int64(i)).UTC()}
		if p.PostedAt.IsZero() || e.PostedAt.Before(p.PostedAt) {
			page = append(page, e)
		}
	}
	return page, nil
}

// TestAPageReadsOneMoreEntryToKnowIfAnotherFollows (FR-44): a page of 2 over 3 entries asks for 3 and gives a
// cursor at its last entry, which the next page asks from; that one, with the third, has no cursor.
func TestAPageReadsOneMoreEntryToKnowIfAnotherFollows(t *testing.T) {
	h := &fakeHistory{n: 3}

	first, err := History(t.Context(), h, ids.New(), ids.New(), "", 2)
	if err != nil {
		t.Fatal(err)
	}
	firstAsked := h.limit
	second, err := History(t.Context(), h, ids.New(), ids.New(), first.Next, 2)
	if err != nil {
		t.Fatal(err)
	}

	if firstAsked != 3 || len(first.Entries) != 2 || first.Next == "" {
		t.Fatalf("first page = %+v after asking for %d, want 2 entries and a cursor after asking for 3", first, firstAsked)
	}
	if want := (Position{PostedAt: first.Entries[1].PostedAt, EntryID: 2}); h.asked != want {
		t.Fatalf("the second page asked from %+v, want the first page's last entry %+v", h.asked, want)
	}
	if len(second.Entries) != 1 || second.Entries[0].EntryID != 1 || second.Next != "" {
		t.Fatalf("second page = %+v, want the third entry and no cursor", second)
	}
}

// TestAFullLastPageHasNoCursor (FR-44): a page of 2 over 2 entries has both and no cursor.
func TestAFullLastPageHasNoCursor(t *testing.T) {
	page, err := History(t.Context(), &fakeHistory{n: 2}, ids.New(), ids.New(), "", 2)

	if err != nil || len(page.Entries) != 2 || page.Next != "" {
		t.Fatalf("a page of 2 over 2 entries = %+v, %v, want both and no cursor", page, err)
	}
}

// TestACursorIsOneTheLedgerIssued: a cursor is the base64 of two varints as cursorOf writes them, of a time
// from 1970 and an entry ID from 1; anything else is ErrInvalidPage, and what cursorOf writes, positionOf
// reads back.
func TestACursorIsOneTheLedgerIssued(t *testing.T) {
	of := func(raw ...byte) string { return base64.RawURLEncoding.EncodeToString(raw) }
	for name, cursor := range map[string]string{
		"not base64":                   "not a cursor!",
		"two varints, then not base64": of(2, 0x82, 0x01) + "!",
		"a varint that overflows":      of(0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff),
		"one varint":                   of(2),
		"bytes left over":              of(2, 2, 2),
		"before 1970":                  of(1, 2),
		"entry ID 0":                   of(2, 0),
		"a varint written longer":      of(0x82, 0x00, 2),
		"base64 with a line break":     of(2, 2)[:1] + "\n" + of(2, 2)[1:],
	} {
		if _, err := positionOf(cursor); !errors.Is(err, ErrInvalidPage) {
			t.Errorf("%s = %v, want ErrInvalidPage", name, err)
		}
	}
	p := Position{PostedAt: time.Date(2026, time.March, 2, 15, 4, 5, 123456000, time.UTC), EntryID: 1 << 40}
	if got, err := positionOf(cursorOf(p)); err != nil || got != p {
		t.Fatalf("positionOf(cursorOf(%+v)) = %+v, %v", p, got, err)
	}
}
