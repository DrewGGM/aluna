package app

import (
	"context"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
)

// ReasonScreeningExpired is why an order that waited for compliance too long is rejected: not an ISO 20022 code,
// because nobody refused the payment, it ran out of time.
const ReasonScreeningExpired = "SCREENING_EXPIRED"

// pendingLife is how long an order may wait for compliance before it expires.
const pendingLife = 7 * 24 * time.Hour

// Expiring is what expiring the orders that waited for compliance too long needs, within a transaction.
type Expiring interface {
	Sending
	// LockStalePending locks and returns up to limit outgoing orders pending_screening made before before, the
	// oldest first, skipping those another transaction holds.
	LockStalePending(ctx context.Context, before time.Time, limit int) ([]ids.ID, error)
}

// ExpirePending rejects, as SCREENING_EXPIRED, up to limit outgoing orders still pending_screening pendingLife after
// they were made, at now, and returns how many: an order nobody resolved does not go out days later, when its
// customer may have paid again.
func ExpirePending(ctx context.Context, p Expiring, now time.Time, limit int) (int, error) {
	stale, err := p.LockStalePending(ctx, now.Add(-pendingLife), limit)
	if err != nil {
		return 0, err
	}
	for _, id := range stale {
		o, err := p.LockOrder(ctx, id)
		if err != nil {
			return 0, err
		}
		if err := o.Reject(ReasonScreeningExpired); err != nil {
			return 0, err
		}
		if err := p.Update(ctx, o); err != nil {
			return 0, err
		}
	}
	return len(stale), nil
}
