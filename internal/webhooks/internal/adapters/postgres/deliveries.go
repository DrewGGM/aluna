package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/webhooks/internal/app"
)

var _ app.Queue = Store{}

// InsertEndpoint stores the entity's endpoint id at url with secret, registered by by; it gets the events of the
// transactions from this one on. It returns when it was registered.
func (s Store) InsertEndpoint(ctx context.Context, entity, id ids.ID, url, secret, by string) (time.Time, error) {
	var at time.Time
	err := s.q.QueryRow(ctx, `INSERT INTO webhooks.endpoints (id, legal_entity_id, url, secret, created_by) VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at`, id, entity, url, secret, by).Scan(&at)
	if err != nil {
		return time.Time{}, fmt.Errorf("store webhook endpoint: %w", err)
	}
	return at, nil
}

// RevokeEndpoint revokes the entity's endpoint id, by by, and fails its pending deliveries; revoking it again keeps
// when and by whom it was. It returns the endpoint's URL and when it was revoked, or app.ErrEndpointNotFound. The
// endpoint's row lock orders it with Queue: the deliveries Queue inserts before it are failed here, and Queue inserts
// none after it.
func (s Store) RevokeEndpoint(ctx context.Context, entity, id ids.ID, by string) (url string, revokedAt time.Time, err error) {
	err = s.q.QueryRow(ctx, `UPDATE webhooks.endpoints
		SET revoked_at = coalesce(revoked_at, clock_timestamp()), revoked_by = coalesce(revoked_by, $3)
		WHERE id = $1 AND legal_entity_id = $2 RETURNING url, revoked_at`, id, entity, by).Scan(&url, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, fmt.Errorf("%w: %s", app.ErrEndpointNotFound, id)
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("revoke webhook endpoint %s: %w", id, err)
	}
	if _, err := s.q.Exec(ctx, `UPDATE webhooks.deliveries SET status = 'failed', last_error = 'the endpoint was revoked'
		WHERE endpoint_id = $1 AND status = 'pending'`, id); err != nil {
		return "", time.Time{}, fmt.Errorf("fail the deliveries of revoked endpoint %s: %w", id, err)
	}
	return url, revokedAt, nil
}

// Subscriptions are every endpoint not revoked, with how far it got.
func (s Store) Subscriptions(ctx context.Context) ([]app.Subscription, error) {
	rows, err := s.q.Query(ctx, `SELECT id, legal_entity_id, after_xact, after_id FROM webhooks.endpoints WHERE revoked_at IS NULL
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("read the webhook endpoints: %w", err)
	}
	subscriptions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (app.Subscription, error) {
		var e app.Subscription
		err := row.Scan(&e.EndpointID, &e.LegalEntityID, &e.After.Xact, &e.After.ID)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the webhook endpoints: %w", err)
	}
	return subscriptions, nil
}

// Queue creates s's deliveries, due at due, and moves s on to after (app.Queue); it creates none if s's endpoint was
// revoked meanwhile. An event already queued for it is not queued again.
func (s Store) Queue(ctx context.Context, sub app.Subscription, deliveries []app.Queued, due time.Time, after app.Position) error {
	moved, err := s.q.Exec(ctx, `UPDATE webhooks.endpoints SET after_xact = $2::xid8, after_id = $3 WHERE id = $1 AND revoked_at IS NULL`,
		sub.EndpointID, after.Xact, after.ID)
	if err != nil {
		return fmt.Errorf("move endpoint %s on: %w", sub.EndpointID, err)
	}
	if moved.RowsAffected() == 0 {
		return nil
	}
	for _, d := range deliveries {
		if _, err := s.q.Exec(ctx, `INSERT INTO webhooks.deliveries (id, endpoint_id, event_id, body, next_attempt_at) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (endpoint_id, event_id) DO NOTHING`, ids.New(), sub.EndpointID, d.EventID, d.Body, due); err != nil {
			return fmt.Errorf("queue event %s for endpoint %s: %w", d.EventID, sub.EndpointID, err)
		}
	}
	return nil
}

// Claim takes up to batch pending deliveries due at now, oldest first, and makes them due again at until: another run
// does not take them while this one sends them, and if it stops, they are attempted again then.
func (s Store) Claim(ctx context.Context, now, until time.Time, batch int) ([]app.Delivery, error) {
	rows, err := s.q.Query(ctx, `UPDATE webhooks.deliveries d SET next_attempt_at = $2
		FROM webhooks.endpoints e
		WHERE e.id = d.endpoint_id AND d.id IN (SELECT id FROM webhooks.deliveries
			WHERE status = 'pending' AND next_attempt_at <= $1 ORDER BY next_attempt_at, id LIMIT $3 FOR UPDATE SKIP LOCKED)
		RETURNING d.id, e.url, e.secret, d.body, d.attempts`, now, until, batch)
	if err != nil {
		return nil, fmt.Errorf("claim the due deliveries: %w", err)
	}
	deliveries, err := pgx.CollectRows(rows, pgx.RowToStructByPos[app.Delivery])
	if err != nil {
		return nil, fmt.Errorf("claim the due deliveries: %w", err)
	}
	return deliveries, nil
}

// Pending is whether delivery id is still pending: a claimed one stops being so if its endpoint is revoked.
func (s Store) Pending(ctx context.Context, id ids.ID) (bool, error) {
	var pending bool
	if err := s.q.QueryRow(ctx, `SELECT status = 'pending' FROM webhooks.deliveries WHERE id = $1`, id).Scan(&pending); err != nil {
		return false, fmt.Errorf("read delivery %s: %w", id, err)
	}
	return pending, nil
}

// Record stores attempt a of delivery id and where the delivery stands after it, if it is still pending and no other
// run recorded that attempt meanwhile: one whose endpoint was revoked while it was sent stays failed.
func (s Store) Record(ctx context.Context, id ids.ID, a app.Attempt) error {
	var lastStatus *int
	if a.Result.Status != 0 {
		lastStatus = &a.Result.Status
	}
	var lastError *string
	if a.Result.Error != "" {
		lastError = &a.Result.Error
	}
	var deliveredAt *time.Time
	if a.Status == app.StatusDelivered {
		deliveredAt = &a.At
	}
	_, err := s.q.Exec(ctx, `UPDATE webhooks.deliveries SET status = $2, attempts = $3, next_attempt_at = $4, last_status = $5,
			last_error = $6, delivered_at = $7
		WHERE id = $1 AND attempts = $3 - 1 AND status = 'pending'`, id, a.Status, a.N, a.Due, lastStatus, lastError, deliveredAt)
	if err != nil {
		return fmt.Errorf("record attempt %d of delivery %s: %w", a.N, id, err)
	}
	return nil
}
