package compliance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/DrewGGM/aluna/internal/compliance/internal/adapters/identity"
	pg "github.com/DrewGGM/aluna/internal/compliance/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/compliance/internal/adapters/sources"
	"github.com/DrewGGM/aluna/internal/compliance/internal/app"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// ListScreeningConfig is how the service screens against the restrictive lists themselves (ADR-0018).
type ListScreeningConfig struct {
	// IdentityURL and IdentityToken are the entity's identity service, which says who a subject is.
	IdentityURL, IdentityToken string
	// Threshold is the score, from 0.5 to 1, from which a name matches one of a list's.
	Threshold float64
	// MaxAge is how old a list may be, up to a week, before screening refuses it and fails closed.
	MaxAge time.Duration
	// Client is the identity service's client; the Screener's limit bounds a call.
	Client *http.Client
	// Log is where a failed reading of the lists is logged.
	Log *slog.Logger
}

// errListScreening means a ListScreeningConfig that cannot screen: a threshold out of range (or not a number), a
// maximum age that is none or longer than a week, which would screen against lists no longer refreshed, or no
// client or log.
var errListScreening = errors.New("invalid list screening")

// listsPoll is how often the service reads the lists again: a refresh reaches screening within a minute; and
// maxListAge the longest a list may go unrefreshed and still screen.
const (
	listsPoll  = time.Minute
	maxListAge = 7 * 24 * time.Hour
)

// NewWithLists creates the service that screens against the lists in q, the pool, as l says (ADR-0018); it
// publishes its events to events. It loads the lists now, failing if it cannot read them, and again every minute
// until ctx ends: screening never reads them within the transaction of the operation it screens.
func NewWithLists(ctx context.Context, events Events, q postgres.Querier, l ListScreeningConfig) (*Service, error) {
	if !(l.Threshold >= 0.5 && l.Threshold <= 1) || l.MaxAge <= 0 || l.MaxAge > maxListAge || l.Client == nil || l.Log == nil {
		return nil, fmt.Errorf("%w: a threshold of %v and a maximum age of %s (with a client: %t; with a log: %t)", errListScreening,
			l.Threshold, l.MaxAge, l.Client != nil, l.Log != nil)
	}
	who, err := identity.New(l.IdentityURL, l.IdentityToken, l.Client)
	if err != nil {
		return nil, err
	}
	lists := pg.NewWatchlist(q)
	if err := lists.Load(ctx); err != nil {
		return nil, err
	}
	go lists.Poll(ctx, listsPoll, l.Log)
	return newService(app.ListScreening{Identities: who, Lists: lists, Threshold: l.Threshold, MaxAge: l.MaxAge, Now: time.Now}, events), nil
}

// ListSources are where the lists are downloaded from, through Client.
type ListSources struct {
	OFACSDN, OFACAlt, UN, PEP string
	Client                    *http.Client
}

// OfficialListSources are the lists where their owners publish them: OFAC's Sanctions List Service, the UN
// Security Council's consolidated list and Función Pública's PEPs in datos.gov.co.
func OfficialListSources(client *http.Client) ListSources {
	return ListSources{
		OFACSDN: "https://sanctionslistservice.ofac.treas.gov/api/PublicationPreview/exports/SDN.CSV",
		OFACAlt: "https://sanctionslistservice.ofac.treas.gov/api/PublicationPreview/exports/ALT.CSV",
		UN:      "https://scsanctions.un.org/resources/xml/en/consolidated.xml",
		PEP:     "https://www.datos.gov.co/resource/3qxn-uc22.json",
		Client:  client,
	}
}

// RefreshLists downloads each list from s and stores, within uow, what changed (ADR-0018): a list that cannot be
// downloaded keeps what it had, which screening refuses once it is too old, and is the error, with the others
// refreshed all the same.
func RefreshLists(ctx context.Context, uow *postgres.UnitOfWork, s ListSources) error {
	return app.RefreshLists(ctx, pg.NewLists(uow), []app.Source{
		sources.OFAC(s.Client, s.OFACSDN, s.OFACAlt), sources.UN(s.Client, s.UN), sources.PEP(s.Client, s.PEP),
	})
}
