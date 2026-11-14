-- The expiry of the payments that wait for compliance (payments.pending.expire, hourly) looks for the oldest
-- outgoing orders still pending_screening: few among all the orders, which only grow.

-- +goose Up
CREATE INDEX orders_pending_by_age ON payments.orders (created_at)
    WHERE direction = 'outgoing' AND status = 'pending_screening';

-- +goose Down
DROP INDEX payments.orders_pending_by_age;
