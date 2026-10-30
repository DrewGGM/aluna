// Package access es la identidad y la autorización de quien llama al core (ADR-0014): el principal que
// sale de un token OIDC ya verificado.
package access

import (
	"context"

	"github.com/DrewGGM/aluna/internal/ids"
)

// Principal es quien hace el request, tal como lo dice su token: Subject es el sujeto del IAM,
// LegalEntityID la entidad legal con la que opera (FR-41: sale del token, nunca de un header), PartyID el
// cliente al que representa un canal (nil si es un operador) y Roles sus roles.
type Principal struct {
	Subject       string
	LegalEntityID ids.ID
	PartyID       *ids.ID
	Roles         []string
}

type principalKey struct{}

// WithPrincipal es ctx con el principal p del request.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom es el principal del request; false si el request no se autenticó.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
