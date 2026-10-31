package deposits

import (
	"context"
	"errors"
	"fmt"

	"github.com/DrewGGM/aluna/internal/calendar"
	pg "github.com/DrewGGM/aluna/internal/deposits/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/deposits/internal/app"
	"github.com/DrewGGM/aluna/internal/deposits/internal/domain"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// typeDormant is the event of an account that turned dormant (architecture §8).
const typeDormant = "aluna.deposits.account.dormant.v1"

// MarkDormant marks dormant, in every entity, the active accounts whose customer has not moved them for more
// than their product's dormancy days, counted to the entity's business date (FR-27), in batches of batch
// accounts, each batch in its own transaction, and emits aluna.deposits.account.dormant.v1 for each. An
// account locked by a movement is left for the next run, and one already dormant is not marked again. An
// entity that fails does not stop the others: their errors come back together. It returns how many it
// marked, counting only committed batches.
func (s *Service) MarkDormant(ctx context.Context, uow *postgres.UnitOfWork, batch int) (int, error) {
	if batch < 1 {
		return 0, fmt.Errorf("dormancy batch %d: must be positive", batch)
	}
	var entities []ids.ID
	if err := uow.Do(ctx, func(tx postgres.Tx) error {
		var err error
		entities, err = pg.NewStore(tx).OpenEntities(ctx)
		return err
	}); err != nil {
		return 0, err
	}
	marked := 0
	var errs []error
	for _, entity := range entities {
		n, err := s.markEntityDormant(ctx, uow, entity, batch)
		marked += n
		if err != nil {
			errs = append(errs, fmt.Errorf("mark dormant in entity %s: %w", entity, err))
		}
	}
	return marked, errors.Join(errs...)
}

// markEntityDormant marks dormant the entity's inactive accounts, product version by version; a version that
// fails does not stop the others.
func (s *Service) markEntityDormant(ctx context.Context, uow *postgres.UnitOfWork, entity ids.ID, batch int) (int, error) {
	var today calendar.Date
	var versions []ids.ID
	if err := uow.Do(ctx, func(tx postgres.Tx) error {
		var err error
		if today, err = s.calendar.BusinessDate(ctx, tx, entity); err != nil {
			return err
		}
		versions, err = pg.NewStore(tx).VersionsInUse(ctx, entity)
		return err
	}); err != nil {
		return 0, err
	}
	marked := 0
	var errs []error
	for _, version := range versions {
		n, err := s.markVersionDormant(ctx, uow, entity, version, today, batch)
		marked += n
		if err != nil {
			errs = append(errs, fmt.Errorf("product version %s: %w", version, err))
		}
	}
	return marked, errors.Join(errs...)
}

// markVersionDormant marks dormant the inactive accounts of a product version, with its dormancy days, in
// batches, in id order, each in its own transaction, until one comes out incomplete.
func (s *Service) markVersionDormant(ctx context.Context, uow *postgres.UnitOfWork, entity, version ids.ID, today calendar.Date, batch int) (int, error) {
	var days int
	if err := uow.Do(ctx, func(tx postgres.Tx) error {
		v, err := s.products.Version(ctx, tx, version)
		if err != nil {
			return err
		}
		days = v.Definition.DormancyDays
		return nil
	}); err != nil {
		return 0, err
	}
	marked := 0
	var after ids.ID
	for {
		var n int
		var last ids.ID
		err := uow.Do(ctx, func(tx postgres.Tx) error { // a retried batch starts again from the same after
			var err error
			d := dormancy{Store: pg.NewStore(tx), s: s, tx: tx}
			n, last, err = app.MarkDormantBatch(ctx, d, entity, version, today, days, after, batch)
			return err
		})
		if err != nil {
			return marked, err
		}
		marked, after = marked+n, last
		if n < batch {
			return marked, nil
		}
	}
}

// dormancy is app.Dormancy over tx; the store gives Inactive and SetStatus.
type dormancy struct {
	pg.Store
	s  *Service
	tx postgres.Tx
}

func (d dormancy) EmitDormant(ctx context.Context, a domain.Account) error {
	return d.s.events.Emit(ctx, d.tx, outbox.Event{
		Type: typeDormant, Subject: "deposit-accounts/" + a.ID.String(), LegalEntityID: a.LegalEntityID, SchemaVersion: 1,
		Data: dormantPayload{
			AccountID:              a.ID,
			LastCustomerActivityOn: isoDate(a.LastCustomerActivityOn),
		},
	})
}

type dormantPayload struct {
	AccountID              ids.ID `json:"account_id"`
	LastCustomerActivityOn string `json:"last_customer_activity_on"`
}
