package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/DrewGGM/aluna/internal/ids"
)

// BreakStatus is where a break stands.
type BreakStatus string

// A break stays open until someone resolves it, with a cause.
const (
	BreakOpen     BreakStatus = "open"
	BreakResolved BreakStatus = "resolved"
)

// MaxPageSize is the most breaks a page has.
const MaxPageSize = 200

const maxCause = 1000

// Errors of the breaks.
var (
	// ErrBreakNotFound means the entity has no such break.
	ErrBreakNotFound = errors.New("break not found")
	// ErrBreakResolved means the break was already resolved.
	ErrBreakResolved = errors.New("the break is already resolved")
	// ErrInvalidResolution means a resolution without a cause, or with one that is not text of up to 1000
	// characters.
	ErrInvalidResolution = errors.New("invalid resolution")
	// ErrInvalidPage means a page of no breaks or of more than MaxPageSize, of another status, or a cursor that
	// is not one a page gave.
	ErrInvalidPage = errors.New("invalid page")
)

// Resolution is why a break closed, who closed it and when; the database says when.
type Resolution struct {
	Cause, By string
	At        time.Time
}

// Break is a finding open until someone resolves it, with a cause.
type Break struct {
	ID      ids.ID
	Control Control
	Finding
	Owner       string
	StatementID ids.ID // of a break a statement opened
	Status      BreakStatus
	OpenedAt    time.Time
	Resolution  *Resolution
}

// opener opens breaks.
type opener interface {
	// OpenBreak stores b and reports whether it is new: false if its entity, control and subject have one open.
	OpenBreak(ctx context.Context, b Break) (bool, error)
	// PublishOpened announces that b opened.
	PublishOpened(ctx context.Context, b Break) error
}

// open opens b and announces it if it is new.
func open(ctx context.Context, r opener, b Break) error {
	fresh, err := r.OpenBreak(ctx, b)
	if err != nil || !fresh {
		return err
	}
	return r.PublishOpened(ctx, b)
}

// Resolving is what resolving a break needs.
type Resolving interface {
	// LockBreak locks and returns break id of the entity, or ErrBreakNotFound.
	LockBreak(ctx context.Context, entity, id ids.ID) (Break, error)
	// ResolveBreak stores b's resolution and returns when it took effect.
	ResolveBreak(ctx context.Context, b Break) (time.Time, error)
	PublishResolved(ctx context.Context, b Break) error
}

// Resolve resolves open break id of the entity with its cause, by who.
func Resolve(ctx context.Context, p Resolving, entity, id ids.ID, cause, by string) (Break, error) {
	if strings.TrimSpace(cause) == "" || !validText(cause, maxCause) {
		return Break{}, fmt.Errorf("%w: its cause is text of 1 to %d characters", ErrInvalidResolution, maxCause)
	}
	b, err := p.LockBreak(ctx, entity, id)
	if err != nil {
		return Break{}, err
	}
	if b.Status != BreakOpen {
		return Break{}, fmt.Errorf("%w: %s", ErrBreakResolved, id)
	}
	b.Status, b.Resolution = BreakResolved, &Resolution{Cause: cause, By: by}
	if b.Resolution.At, err = p.ResolveBreak(ctx, b); err != nil {
		return Break{}, err
	}
	if err := p.PublishResolved(ctx, b); err != nil {
		return Break{}, err
	}
	return b, nil
}

// BreakPage is a page of breaks, newest first, and the cursor of the next one: empty on the last page.
type BreakPage struct {
	Breaks []Break
	Next   string
}

// BreakReader reads the breaks.
type BreakReader interface {
	// BreaksBefore are the first limit breaks of the entity with an id below before, or from the newest one if it
	// is zero, those in status if it has one, newest first.
	BreaksBefore(ctx context.Context, entity ids.ID, status BreakStatus, before ids.ID, limit int) ([]Break, error)
}

// Breaks is the page of limit breaks of the entity after cursor, those in status if it has one, or its first page
// when cursor is empty. Ids grow with time, so the last one of a page is the cursor of the next.
func Breaks(ctx context.Context, r BreakReader, entity ids.ID, status BreakStatus, cursor string, limit int) (BreakPage, error) {
	if limit < 1 || limit > MaxPageSize {
		return BreakPage{}, fmt.Errorf("%w: a page has 1 to %d breaks, not %d", ErrInvalidPage, MaxPageSize, limit)
	}
	if status != "" && status != BreakOpen && status != BreakResolved {
		return BreakPage{}, fmt.Errorf("%w: a break is open or resolved", ErrInvalidPage)
	}
	var before ids.ID
	if cursor != "" {
		parsed, err := ids.Parse(cursor)
		if err != nil {
			return BreakPage{}, fmt.Errorf("%w: unknown cursor", ErrInvalidPage)
		}
		before = parsed
	}
	found, err := r.BreaksBefore(ctx, entity, status, before, limit+1)
	if err != nil {
		return BreakPage{}, err
	}
	if len(found) <= limit {
		return BreakPage{Breaks: found}, nil
	}
	return BreakPage{Breaks: found[:limit], Next: found[limit-1].ID.String()}, nil
}

// validText reports whether s is text of up to most characters that PostgreSQL takes: no NUL.
func validText(s string, most int) bool {
	return utf8.ValidString(s) && utf8.RuneCountInString(s) <= most && !strings.ContainsRune(s, 0)
}
