package app

import (
	"context"
	"time"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/deposits/internal/domain"
	"github.com/DrewGGM/aluna/internal/ids"
)

// dormancyActor is who records the change to dormant: the dormancy job, not a person.
const dormancyActor = "deposits.dormancy"

// Dormancy is what marking accounts dormant needs, within the transaction a batch runs in.
type Dormancy interface {
	// Inactive locks (FOR UPDATE SKIP LOCKED: one being moved is left for a later run), in id order after
	// after, up to batch active accounts of the entity with product version versionID whose last customer
	// activity is before before.
	Inactive(ctx context.Context, entity, versionID ids.ID, before calendar.Date, after ids.ID, batch int) ([]domain.Account, error)
	SetStatus(ctx context.Context, a domain.Account, to domain.Status, actor string) error
	EmitDormant(ctx context.Context, a domain.Account) error
}

// dormantBefore is the day before which an account's last customer activity makes it dormant on day today,
// with a product of days dormancy days (FR-27): it has gone more than days days without its customer moving
// it. Exactly days days is not more.
func dormantBefore(today calendar.Date, days int) calendar.Date {
	return dateOfTime(time.Date(today.Year, today.Month, today.Day-days, 0, 0, 0, 0, time.UTC))
}

// MarkDormantBatch marks dormant a batch of the entity's active accounts of product version versionID that
// went more than days days without customer activity by day today (FR-27), and returns how many it marked
// (fewer than batch when no more are due or the rest are locked) and the last one's id, where the next batch
// starts. A capitalization or any other credit that is not the customer's doing does not move the last
// activity; an account already dormant is not active, so running it again marks nothing.
func MarkDormantBatch(ctx context.Context, s Dormancy, entity, versionID ids.ID, today calendar.Date, days int, after ids.ID, batch int) (int, ids.ID, error) {
	inactive, err := s.Inactive(ctx, entity, versionID, dormantBefore(today, days), after, batch)
	if err != nil || len(inactive) == 0 {
		return 0, after, err
	}
	for _, a := range inactive {
		if err := s.SetStatus(ctx, a, domain.Dormant, dormancyActor); err != nil {
			return 0, after, err
		}
		if err := s.EmitDormant(ctx, a); err != nil {
			return 0, after, err
		}
	}
	return len(inactive), inactive[len(inactive)-1].ID, nil
}
