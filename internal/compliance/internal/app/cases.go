package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
)

// CaseKind is what a case screened.
type CaseKind string

// Case kinds: the registration of a party and an outgoing payment.
const (
	CaseParty   CaseKind = "party"
	CasePayment CaseKind = "payment"
)

// CaseStatus is where a case stands.
type CaseStatus string

// Case statuses: pending until compliance resolves it, and then cleared (no match) or confirmed. A case the
// vendor cleared is cleared from the start.
const (
	CasePending   CaseStatus = "pending"
	CaseCleared   CaseStatus = "cleared"
	CaseConfirmed CaseStatus = "confirmed"
)

// Case is a screening of an operation's subject (FR-42). SubjectID is the party or the payment order the
// operation is about; VendorRef is empty if the vendor did not answer, and ResolvedBy if nobody had to.
type Case struct {
	ID, LegalEntityID ids.ID
	Kind              CaseKind
	SubjectID         ids.ID
	Status            CaseStatus
	VendorRef         string
	CreatedAt         time.Time
	ResolvedBy        string
}

var (
	// ErrCaseNotFound means the case does not exist in the entity.
	ErrCaseNotFound = errors.New("screening case not found")
	// ErrCaseResolved means the case is no longer pending.
	ErrCaseResolved = errors.New("screening case already resolved")
)

// Cases is the persistence of screening cases; reads by ID are of the entity's cases, ErrCaseNotFound
// otherwise.
type Cases interface {
	InsertCase(ctx context.Context, c Case) (Case, error)
	Case(ctx context.Context, entity, id ids.ID) (Case, error)
	// LockCase reads the case and locks it until the transaction ends.
	LockCase(ctx context.Context, entity, id ids.ID) (Case, error)
	ResolveCase(ctx context.Context, c Case) error
}

// Open screens subject and records the case of kind about subjectID in entity: cleared if the vendor
// cleared it, pending otherwise.
func Open(ctx context.Context, cases Cases, s *Screener, entity ids.ID, kind CaseKind, subjectID ids.ID, subject Subject) (Case, error) {
	o, ref := s.Screen(ctx, subject)
	status := CaseCleared
	if o != OutcomeClear {
		status = CasePending
	}
	return cases.InsertCase(ctx, Case{ID: ids.New(), LegalEntityID: entity, Kind: kind, SubjectID: subjectID, Status: status, VendorRef: ref})
}

// CheckResolution reports whether case id of entity can still be resolved: ErrCaseResolved if it is no
// longer pending.
func CheckResolution(ctx context.Context, cases Cases, entity, id ids.ID) error {
	c, err := cases.Case(ctx, entity, id)
	if err != nil {
		return err
	}
	return pending(c)
}

// Resolve resolves case id of entity as to, CaseCleared or CaseConfirmed, by who, under its lock: a second
// resolution waits for the first and finds it resolved (ErrCaseResolved).
func Resolve(ctx context.Context, cases Cases, entity, id ids.ID, to CaseStatus, by string) (Case, error) {
	c, err := cases.LockCase(ctx, entity, id)
	if err != nil {
		return Case{}, err
	}
	if err := pending(c); err != nil {
		return Case{}, err
	}
	c.Status, c.ResolvedBy = to, by
	if err := cases.ResolveCase(ctx, c); err != nil {
		return Case{}, err
	}
	return c, nil
}

func pending(c Case) error {
	if c.Status != CasePending {
		return fmt.Errorf("%w: case %s is %s", ErrCaseResolved, c.ID, c.Status)
	}
	return nil
}
