package payments

import (
	"context"
	"time"

	"github.com/DrewGGM/aluna/internal/payments/internal/app"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// ExpirePending rejects, as SCREENING_EXPIRED, the outgoing orders of every entity still pending_screening 7 days
// after they were made, at now, in batches of batch, each in its own transaction within uow, and returns how many.
// Its job runs it every hour; resolving the case of an expired order later leaves the order as it is.
func (s *Service) ExpirePending(ctx context.Context, uow *postgres.UnitOfWork, now time.Time, batch int) (int, error) {
	total := 0
	for {
		var n int
		err := uow.Do(ctx, func(tx postgres.Tx) error {
			var err error
			n, err = app.ExpirePending(ctx, orders{s: s, tx: tx}, now, batch)
			return err
		})
		if err != nil {
			return total, err
		}
		total += n
		if n < batch {
			return total, nil
		}
	}
}
