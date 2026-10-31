-- The read does not lock the row: a concurrent rollover does not wait for postings that already read it.
-- name: BusinessDate :one
SELECT business_date FROM calendar.legal_entities WHERE id = $1;

-- Advances only if the date is still the expected one: two rollovers from the same date advance once.
-- name: AdvanceBusinessDate :execrows
UPDATE calendar.legal_entities SET business_date = @next WHERE id = @id AND business_date = @expected;

-- name: PeriodStatus :one
SELECT status FROM calendar.periods WHERE legal_entity_id = @legal_entity_id AND period = @period;
