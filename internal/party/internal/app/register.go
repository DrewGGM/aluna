// Package app holds party's use cases and the ports they need.
package app

import (
	"context"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/party/internal/domain"
)

// Screener screens a party before it is registered (FR-42). The facade implements it over its party.Screener,
// which compliance implements: party does not import compliance, which depends on party.
type Screener interface {
	// ScreenParty reports whether screening cleared p; anything else, a possible match or no answer, holds it.
	ScreenParty(ctx context.Context, p domain.Party) (cleared bool, err error)
}

// Registration is what Register needs: screening and storing the party.
type Registration interface {
	Screener
	Insert(ctx context.Context, p domain.Party) (domain.Party, error)
}

// Register registers the party of in after screening it: its first version is KYC pending if screening
// cleared it, and pending_screening otherwise, until compliance resolves its case.
func Register(ctx context.Context, r Registration, in domain.Registration) (domain.Party, error) {
	p, err := domain.NewParty(ids.New(), in)
	if err != nil {
		return domain.Party{}, err
	}
	cleared, err := r.ScreenParty(ctx, p)
	if err != nil {
		return domain.Party{}, err
	}
	if !cleared {
		p = p.HeldForScreening()
	}
	return r.Insert(ctx, p)
}
