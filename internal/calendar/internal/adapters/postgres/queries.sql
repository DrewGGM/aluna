-- The read does not lock the row: a concurrent rollover does not wait for postings that already read it.
-- name: BusinessDate :one
SELECT business_date FROM calendar.legal_entities WHERE id = $1;

-- Advances only if the date is still the expected one: two rollovers from the same date advance once.
-- name: AdvanceBusinessDate :execrows
UPDATE calendar.legal_entities SET business_date = @next WHERE id = @id AND business_date = @expected;

-- name: PeriodStatus :one
SELECT status FROM calendar.periods WHERE legal_entity_id = @legal_entity_id AND period = @period;

-- Takes the period's advisory lock until the transaction ends, exclusive: the postings in flight in it end first,
-- and the rest wait for the close (migration 0029 takes it shared on every transaction).
-- name: LockPeriod :exec
SELECT pg_advisory_xact_lock(calendar.period_lock_key(@legal_entity_id, @period));

-- A period without a row is open; one that is not open stays as it is, and no row changes.
-- name: MarkPeriodClosing :execrows
INSERT INTO calendar.periods (legal_entity_id, period, status) VALUES (@legal_entity_id, @period, 'closing')
ON CONFLICT (legal_entity_id, period) DO UPDATE SET status = 'closing' WHERE calendar.periods.status = 'open';

-- name: MarkPeriodClosed :exec
UPDATE calendar.periods SET status = 'closed', closed_by = @closed_by
WHERE legal_entity_id = @legal_entity_id AND period = @period;
