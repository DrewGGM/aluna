// Package access is the identity and authorization of whoever calls the core (ADR-0014): the principal
// taken from an already verified OIDC token.
package access

import (
	"context"

	"github.com/DrewGGM/aluna/internal/ids"
)

// Principal is whoever makes the request, as its token says: Subject is the IAM subject, LegalEntityID the
// legal entity it operates with (FR-41: it comes from the token, never from a header), PartyID the customer
// a channel represents (nil for an operator) and Roles its roles.
type Principal struct {
	Subject       string
	LegalEntityID ids.ID
	PartyID       *ids.ID
	Roles         []string
}

type principalKey struct{}

// WithPrincipal is ctx with the request's principal p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom is the request's principal; false if the request was not authenticated.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
