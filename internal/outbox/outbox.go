// Package outbox publishes domain events (ADR-0007): the producer writes the event in the same
// transaction as its effect, along with a River job that later delivers it to the internal consumers.
// Delivery is at-least-once; each consumer records in infra.inbox the events it already processed, in
// the same transaction as its effect, so it processes each event once.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// RiverSchema is the schema of River's tables (migration 0009); every River client in Aluna uses it.
const RiverSchema = "infra"

// purgeBatch bounds each Purge DELETE so that it fits in the pool's statement_timeout.
const purgeBatch = 10_000

// Handler processes an event within tx, the same transaction that records it in the inbox.
type Handler func(ctx context.Context, tx postgres.Tx, e StoredEvent) error

// Outbox emits events and delivers them to their consumers.
type Outbox struct {
	pool *pgxpool.Pool
	uow  *postgres.UnitOfWork
	jobs *river.Client[pgx.Tx] // only inserts jobs: the worker's client works them
	subs []subscription
}

type subscription struct {
	consumer string
	types    []string
	handle   Handler
}

// New creates the outbox over pool.
func New(pool *pgxpool.Pool) (*Outbox, error) {
	jobs, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: RiverSchema})
	if err != nil {
		return nil, fmt.Errorf("outbox river client: %w", err)
	}
	return &Outbox{pool: pool, uow: postgres.NewUnitOfWork(pool), jobs: jobs}, nil
}

// Emit stores e within tx, with the job that will publish it: if tx rolls back, neither the event nor
// the job remains.
func (o *Outbox) Emit(ctx context.Context, tx postgres.Tx, e Event) error {
	if e.ID == (ids.ID{}) {
		e.ID = ids.New()
	}
	payload, err := json.Marshal(e.Data)
	if err != nil {
		return fmt.Errorf("event %s data: %w", e.Type, err)
	}
	var correlation *ids.ID
	if e.CorrelationID != (ids.ID{}) {
		correlation = &e.CorrelationID
	}
	_, err = tx.Exec(ctx, `INSERT INTO infra.outbox (id, type, subject, legal_entity_id, correlation_id, schema_version, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, e.ID, e.Type, e.Subject, e.LegalEntityID, correlation, e.SchemaVersion, payload)
	if err != nil {
		return fmt.Errorf("store event %s: %w", e.Type, err)
	}
	if _, err := o.jobs.InsertTx(ctx, tx.Pgx(), publishArgs{EventID: e.ID}, nil); err != nil {
		return fmt.Errorf("enqueue publication of event %s: %w", e.ID, err)
	}
	return nil
}

// Subscribe registers the consumer for the given types. It is called at startup, before the worker
// starts publishing.
func (o *Outbox) Subscribe(consumer string, types []string, h Handler) {
	o.subs = append(o.subs, subscription{consumer: consumer, types: types, handle: h})
}

// AddWorkers registers in w the worker that publishes the events (outbox.publish).
func (o *Outbox) AddWorkers(w *river.Workers) { river.AddWorker(w, &publishWorker{o: o}) }

// Purge deletes the events published before before (ADR-0007: the Events API retains them for 30 days),
// with their inbox rows, and reports how many it deleted. It goes in batches in id order (UUIDv7, grows
// over time): old published events sit at the start of the primary key and full batches stop early; the
// last one scans the table (Q-81). It skips events locked by an ongoing publication: the next purge will
// delete them.
func (o *Outbox) Purge(ctx context.Context, before time.Time) (int64, error) {
	var total int64
	for {
		tag, err := o.pool.Exec(ctx, `DELETE FROM infra.outbox WHERE id IN (
			SELECT id FROM infra.outbox WHERE published_at < $1 ORDER BY id LIMIT $2 FOR UPDATE SKIP LOCKED)`, before, purgeBatch)
		if err != nil {
			return total, fmt.Errorf("purge outbox: %w", err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < purgeBatch {
			return total, nil
		}
	}
}

type publishArgs struct {
	EventID ids.ID `json:"event_id"`
}

func (publishArgs) Kind() string { return "outbox.publish" }

type publishWorker struct {
	river.WorkerDefaults[publishArgs]
	o *Outbox
}

// Work delivers the event to each consumer subscribed to its type, each in its own transaction, and marks
// it published. A failing consumer does not stop the others: the job is retried and those that already
// processed it skip it.
func (w *publishWorker) Work(ctx context.Context, job *river.Job[publishArgs]) error {
	e, err := w.o.load(ctx, job.Args.EventID)
	if err != nil {
		return err
	}
	var failed []error
	for _, s := range w.o.subs {
		if !slices.Contains(s.types, e.Type) {
			continue
		}
		if err := w.o.uow.Do(ctx, func(tx postgres.Tx) error { return deliver(ctx, tx, s, e) }); err != nil {
			failed = append(failed, fmt.Errorf("consumer %s, event %s: %w", s.consumer, e.ID, err))
		}
	}
	if len(failed) > 0 {
		return errors.Join(failed...)
	}
	if _, err := w.o.pool.Exec(ctx, `UPDATE infra.outbox SET published_at = clock_timestamp() WHERE id = $1 AND published_at IS NULL`, e.ID); err != nil {
		return fmt.Errorf("mark event %s published: %w", e.ID, err)
	}
	return nil
}

// deliver records the event in the consumer's inbox and, if it is the first time, processes it in the
// same tx. Two concurrent deliveries: the second waits for the first one's INSERT and, if that commits,
// does not insert.
func deliver(ctx context.Context, tx postgres.Tx, s subscription, e StoredEvent) error {
	tag, err := tx.Exec(ctx, `INSERT INTO infra.inbox (consumer, event_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, s.consumer, e.ID)
	if err != nil {
		return fmt.Errorf("record in inbox: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	return s.handle(ctx, tx, e)
}

func (o *Outbox) load(ctx context.Context, id ids.ID) (StoredEvent, error) {
	var e StoredEvent
	var correlation *ids.ID
	var payload []byte
	err := o.pool.QueryRow(ctx, `SELECT id, type, subject, legal_entity_id, correlation_id, schema_version, payload, created_at
		FROM infra.outbox WHERE id = $1`, id).Scan(&e.ID, &e.Type, &e.Subject, &e.LegalEntityID, &correlation, &e.SchemaVersion, &payload, &e.Time)
	if err != nil {
		return StoredEvent{}, fmt.Errorf("read event %s: %w", id, err)
	}
	if correlation != nil {
		e.CorrelationID = *correlation
	}
	e.Data = payload
	return e, nil
}
