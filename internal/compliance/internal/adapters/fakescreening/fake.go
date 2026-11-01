// Package fakescreening is the screening vendor of development and tests, until one is chosen (Q-16, D-2).
// It clears every subject except those whose external reference starts with one of its prefixes.
package fakescreening

import (
	"context"
	"errors"
	"strings"

	"github.com/DrewGGM/aluna/internal/compliance/internal/app"
)

// Prefixes of the external reference that make the vendor answer otherwise.
const (
	MatchPrefix = "match-" // a possible match
	ErrorPrefix = "error-" // a failure
	SlowPrefix  = "slow-"  // no answer until the context ends
)

// errUnavailable is the vendor failing; the Screener does not tell one failure from another.
var errUnavailable = errors.New("fake screening vendor unavailable")

// Vendor is the fake vendor.
type Vendor struct{}

// Screen answers as the subject's external reference says; its reference to the check is the subject's ID.
func (Vendor) Screen(ctx context.Context, s app.Subject) (app.Result, error) {
	ref := s.ExternalRef
	switch {
	case strings.HasPrefix(ref, MatchPrefix):
		return app.Result{Status: app.StatusPossibleMatch, Ref: "fake-" + s.ID.String()}, nil
	case strings.HasPrefix(ref, ErrorPrefix):
		return app.Result{}, errUnavailable
	case strings.HasPrefix(ref, SlowPrefix):
		<-ctx.Done()
		return app.Result{}, ctx.Err()
	}
	return app.Result{Status: app.StatusClear, Ref: "fake-" + s.ID.String()}, nil
}
