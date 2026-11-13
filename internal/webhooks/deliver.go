package webhooks

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
	pg "github.com/DrewGGM/aluna/internal/webhooks/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/webhooks/internal/adapters/sender"
	"github.com/DrewGGM/aluna/internal/webhooks/internal/app"
)

// ErrInvalidEndpoint means a webhook endpoint that is not an https URL of the internet (SSRF).
var ErrInvalidEndpoint = app.ErrInvalidEndpoint

// Endpoint is a registered webhook endpoint and its secret, which only its registration shows.
type Endpoint struct {
	ID        ids.ID
	URL       string
	Secret    string
	CreatedAt time.Time
}

// Sender sends HTTP requests; NewSender is the one to send webhooks with.
type Sender = app.Sender

// NewSender is the client webhooks are sent with: it only connects to public addresses and does not follow redirects.
func NewSender() *http.Client { return sender.New() }

// claimLease is how long a claimed delivery waits before another run may attempt it: longer than a run's sends, a
// batch of attempts of up to 15 seconds each.
const claimLease = 15 * time.Minute

// RegisterEndpoint registers, within tx, an endpoint of the entity at url, by who, with a new secret (FR-46): it gets
// the entity's events from the transactions from this one on, signed with the secret. A URL that is not https or
// whose host is or resolves to an address that is not public is ErrInvalidEndpoint.
func (s *Service) RegisterEndpoint(ctx context.Context, tx postgres.Tx, entity ids.ID, url, by string) (Endpoint, error) {
	if err := app.CheckEndpoint(ctx, net.DefaultResolver, url); err != nil {
		return Endpoint{}, err
	}
	secret, err := app.NewSecret()
	if err != nil {
		return Endpoint{}, err
	}
	e := Endpoint{ID: ids.New(), URL: url, Secret: secret}
	if e.CreatedAt, err = pg.NewStore(tx).InsertEndpoint(ctx, entity, e.ID, url, secret, by); err != nil {
		return Endpoint{}, err
	}
	return e, nil
}

// Deliver queues for each endpoint up to batch new events of its entity and then attempts up to batch due deliveries
// with sender (FR-46): each one is POSTed signed at the time of its attempt until its endpoint answers 2xx, retried up
// to 8 times in 72 hours with an exponential backoff, and then failed. It reads the time from now at each step, and
// sends outside the database's transactions, so a slow endpoint holds none. An endpoint or a delivery that fails does
// not stop the others: their errors come back together.
func (s *Service) Deliver(ctx context.Context, uow *postgres.UnitOfWork, sender Sender, now func() time.Time, batch int) error {
	if batch < 1 {
		return fmt.Errorf("webhooks batch %d: must be positive", batch)
	}
	errs := []error{enqueue(ctx, uow, now(), batch)}
	var claimed []app.Delivery
	if err := uow.Do(ctx, func(tx postgres.Tx) error {
		at := now()
		var err error
		claimed, err = pg.NewStore(tx).Claim(ctx, at, at.Add(claimLease), batch)
		return err
	}); err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, d := range claimed {
		a := app.Attempted(d, app.Send(ctx, sender, d, now()), now())
		if err := uow.Do(ctx, func(tx postgres.Tx) error { return pg.NewStore(tx).Record(ctx, d.ID, a) }); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// enqueue queues for each endpoint the next batch of its entity's events, each in its own transaction.
func enqueue(ctx context.Context, uow *postgres.UnitOfWork, now time.Time, batch int) error {
	var subscriptions []app.Subscription
	if err := uow.Do(ctx, func(tx postgres.Tx) error {
		var err error
		subscriptions, err = pg.NewStore(tx).Subscriptions(ctx)
		return err
	}); err != nil {
		return err
	}
	var errs []error
	for _, sub := range subscriptions {
		if err := uow.Do(ctx, func(tx postgres.Tx) error { return app.Enqueue(ctx, pg.NewStore(tx), sub, now, batch) }); err != nil {
			errs = append(errs, fmt.Errorf("queue the events of endpoint %s: %w", sub.EndpointID, err))
		}
	}
	return errors.Join(errs...)
}
