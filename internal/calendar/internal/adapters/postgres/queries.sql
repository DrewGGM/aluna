-- La lectura no bloquea la fila: un rollover concurrente no espera a los postings que ya la leyeron.
-- name: BusinessDate :one
SELECT business_date FROM calendar.legal_entities WHERE id = $1;

-- Avanza solo si la fecha sigue siendo la esperada: dos rollovers con la misma fecha avanzan una vez.
-- name: AdvanceBusinessDate :execrows
UPDATE calendar.legal_entities SET business_date = @next WHERE id = @id AND business_date = @expected;

-- name: PeriodStatus :one
SELECT status FROM calendar.periods WHERE legal_entity_id = @legal_entity_id AND period = @period;
