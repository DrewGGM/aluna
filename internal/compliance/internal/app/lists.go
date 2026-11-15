package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/DrewGGM/aluna/internal/compliance/internal/domain/matching"
)

// The restrictive lists screening reads (ADR-0018): the sanctions of OFAC (SDN) and of the UN Security Council,
// which bind every subject, and the PEPs of Decreto 830 de 2021, which matter to a party being registered (due
// diligence), not to the beneficiary of a payment.
const (
	ListOFAC = "ofac_sdn"
	ListUN   = "un_consolidated"
	ListPEP  = "pep_co"
)

// lists are all of them: screening refuses to run without any it screens against loaded and recent.
var lists = [...]string{ListOFAC, ListUN, ListPEP}

var (
	// ErrStaleLists means a list that was never loaded or not refreshed recently: screening against it would
	// clear what it might no longer clear, so it fails closed, for every subject.
	ErrStaleLists = errors.New("a restrictive list is missing or out of date")
	// ErrUnknownSubject means a subject the entity's identity service does not know.
	ErrUnknownSubject = errors.New("the identity service does not know the subject")
	// ErrEmptyList means a list that came without entries: it is not stored, the one there was is kept.
	ErrEmptyList = errors.New("a restrictive list came empty")
	// ErrShrunkList means a list that came with less than half the entries it had: a truncated download or a
	// changed format, not a list that shrank. It is not stored, the one there was is kept.
	ErrShrunkList = errors.New("a restrictive list came with less than half its entries")
)

// unknownRef is the reference of the case of a subject the identity service does not know, or knows no name of:
// it waits for compliance, as a possible match does, without counting as the vendor failing, which a caller could
// otherwise repeat until the Screener's circuit opens for every subject.
const unknownRef = "identity:unknown"

// clearRef is the reference of the case of a subject that matched no entry.
const clearRef = "lists"

// ListScreening is the screening vendor that screens against the lists themselves (ADR-0018), as the SFC allows
// an entity to: it asks the identity service who the subject is and matches its names and documents against the
// lists, by document exactly and by name from Threshold up. A list it screens against older than MaxAge fails
// closed.
type ListScreening struct {
	Identities Identities
	Lists      Watchlist
	Threshold  float64
	MaxAge     time.Duration
	Now        func() time.Time
}

// Screen screens s: a possible match if it matches an entry, its Ref naming the entries and why, or else if the
// identity service does not know it or any name of it; clear otherwise. The documents of a subject without a name
// are matched all the same, so that a case never hides a hit behind an unknown identity.
func (l ListScreening) Screen(ctx context.Context, s Subject) (Result, error) {
	loaded, err := l.Lists.Current()
	if err != nil {
		return Result{}, err
	}
	if err := l.recent(loaded, s.Kind); err != nil {
		return Result{}, err
	}
	who, err := l.Identities.Identify(ctx, s)
	switch {
	case errors.Is(err, ErrUnknownSubject):
		return Result{Status: StatusPossibleMatch, Ref: unknownRef}, nil
	case err != nil:
		return Result{}, err
	}
	hits := loaded.Index.Match(who.Names, who.Documents, l.Threshold, func(list string) bool { return screenedAgainst(s.Kind, list) })
	switch {
	case len(hits) > 0:
		return Result{Status: StatusPossibleMatch, Ref: refOf(hits)}, nil
	case !named(who):
		return Result{Status: StatusPossibleMatch, Ref: unknownRef}, nil
	}
	return Result{Status: StatusClear, Ref: clearRef}, nil
}

// recent is ErrStaleLists unless every list a subject of kind is screened against is loaded and refreshed within
// MaxAge: a beneficiary is not held up by the PEPs, which it is not screened against.
func (l ListScreening) recent(loaded LoadedLists, kind SubjectKind) error {
	for _, list := range lists {
		if at, ok := loaded.Refreshed[list]; screenedAgainst(kind, list) && (!ok || l.Now().Sub(at) > l.MaxAge) {
			return fmt.Errorf("%w: %s", ErrStaleLists, list)
		}
	}
	return nil
}

// named reports whether who has a name that is more than punctuation and particles: one that can match.
func named(who Identity) bool {
	return slices.ContainsFunc(who.Names, func(name string) bool { return len(matching.Words(name)) > 0 })
}

// screenedAgainst reports whether a subject of kind is screened against list: a party against every list, a
// beneficiary (its key or its account) against the sanctions only.
func screenedAgainst(kind SubjectKind, list string) bool {
	return kind == SubjectParty || list != ListPEP
}

// maxRef is the longest reference a case keeps of its vendor (compliance.cases.vendor_ref), and moreRef what is
// kept of it for how many entries did not fit (" +9999 more").
const (
	maxRef  = 255
	moreRef = 11
)

// refOf names hits' entries, in order, each with why it matched, its document ("ofac_sdn:4108=document") or the
// score of its closest name ("ofac_sdn:4108~0.95"), as many as fit in maxRef, and how many more there are.
func refOf(hits []matching.Hit) string {
	refs := make([]string, len(hits))
	for i, h := range hits {
		refs[i] = fmt.Sprintf("%s:%s~%.2f", h.List, h.ID, h.Score)
		if h.Document {
			refs[i] = h.List + ":" + h.ID + "=document"
		}
	}
	slices.Sort(refs)
	ref := refs[0]
	for i, r := range refs[1:] {
		if len(ref)+1+len(r) > maxRef-moreRef {
			return fmt.Sprintf("%s +%d more", ref, len(refs)-1-i)
		}
		ref += " " + r
	}
	return ref
}

// Source is where a list comes from: Fetch is its entries now and a version that changes when they do.
type Source struct {
	List  string
	Fetch func(ctx context.Context) (version string, entries []matching.Entry, err error)
}

// RefreshLists fetches each source and stores what changed, so that a list is refreshed even when it is the
// same. A source that fails, or comes empty or shrunk, keeps the list it had, which screening refuses once it is
// too old, and the others are refreshed all the same; its error is returned with theirs.
func RefreshLists(ctx context.Context, store ListStore, sources []Source) error {
	var errs []error
	for _, s := range sources {
		if err := refresh(ctx, store, s); err != nil {
			errs = append(errs, fmt.Errorf("refresh %s: %w", s.List, err))
		}
	}
	return errors.Join(errs...)
}

func refresh(ctx context.Context, store ListStore, s Source) error {
	version, entries, err := s.Fetch(ctx)
	switch {
	case err != nil:
		return err
	case len(entries) == 0:
		return ErrEmptyList
	}
	stored, had, err := store.Stored(ctx, s.List)
	switch {
	case err != nil:
		return err
	case stored == version:
		return store.Touch(ctx, s.List)
	case len(entries) < had/2:
		return fmt.Errorf("%w: %d, it had %d", ErrShrunkList, len(entries), had)
	}
	return store.Replace(ctx, s.List, version, entries)
}
