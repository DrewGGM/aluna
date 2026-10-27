// Package outbox publica los eventos de dominio (ADR-0007): el productor escribe el evento en la misma
// transacción que su efecto, junto con un job River que lo entrega después a los consumidores internos.
// La entrega es at-least-once; cada consumidor anota en infra.inbox los eventos que ya procesó, en la
// misma transacción que su efecto, así que procesa cada evento una vez.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// RiverSchema es el esquema de las tablas de River (migración 0009); todo cliente River de Aluna lo usa.
const RiverSchema = "infra"

// Handler procesa un evento dentro de tx, la misma transacción que lo anota en el inbox.
type Handler func(ctx context.Context, tx postgres.Tx, e StoredEvent) error

// Outbox emite eventos y los entrega a sus consumidores.
type Outbox struct {
	pool *pgxpool.Pool
	uow  *postgres.UnitOfWork
	jobs *river.Client[pgx.Tx] // solo inserta jobs: los trabaja el cliente del worker
	subs []subscription
}

type subscription struct {
	consumer string
	types    []string
	handle   Handler
}

// New crea el outbox sobre pool.
func New(pool *pgxpool.Pool) (*Outbox, error) {
	jobs, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: RiverSchema})
	if err != nil {
		return nil, fmt.Errorf("cliente River del outbox: %w", err)
	}
	return &Outbox{pool: pool, uow: postgres.NewUnitOfWork(pool), jobs: jobs}, nil
}

// Emit guarda e dentro de tx, con el job que lo publicará: si tx se revierte, no queda ni el evento ni el
// job.
func (o *Outbox) Emit(ctx context.Context, tx postgres.Tx, e Event) error {
	if e.ID == (ids.ID{}) {
		e.ID = ids.New()
	}
	payload, err := json.Marshal(e.Data)
	if err != nil {
		return fmt.Errorf("datos del evento %s: %w", e.Type, err)
	}
	var correlation *ids.ID
	if e.CorrelationID != (ids.ID{}) {
		correlation = &e.CorrelationID
	}
	_, err = tx.Exec(ctx, `INSERT INTO infra.outbox (id, type, subject, legal_entity_id, correlation_id, schema_version, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, e.ID, e.Type, e.Subject, e.LegalEntityID, correlation, e.SchemaVersion, payload)
	if err != nil {
		return fmt.Errorf("guardar el evento %s: %w", e.Type, err)
	}
	if _, err := o.jobs.InsertTx(ctx, tx.Pgx(), publishArgs{EventID: e.ID}, nil); err != nil {
		return fmt.Errorf("encolar la publicación del evento %s: %w", e.ID, err)
	}
	return nil
}

// Subscribe registra al consumidor para los tipos dados. Se llama al arrancar, antes de que el worker
// empiece a publicar.
func (o *Outbox) Subscribe(consumer string, types []string, h Handler) {
	o.subs = append(o.subs, subscription{consumer: consumer, types: types, handle: h})
}

// AddWorkers registra en w el worker que publica los eventos (outbox.publish).
func (o *Outbox) AddWorkers(w *river.Workers) { river.AddWorker(w, &publishWorker{o: o}) }

type publishArgs struct {
	EventID ids.ID `json:"event_id"`
}

func (publishArgs) Kind() string { return "outbox.publish" }

type publishWorker struct {
	river.WorkerDefaults[publishArgs]
	o *Outbox
}

// Work entrega el evento a cada consumidor suscrito a su tipo, cada uno en su transacción, y lo marca
// publicado. Un consumidor que falla no detiene a los demás: el job se reintenta y los que ya lo
// procesaron lo saltan.
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
			failed = append(failed, fmt.Errorf("consumidor %s, evento %s: %w", s.consumer, e.ID, err))
		}
	}
	if len(failed) > 0 {
		return errors.Join(failed...)
	}
	if _, err := w.o.pool.Exec(ctx, `UPDATE infra.outbox SET published_at = clock_timestamp() WHERE id = $1 AND published_at IS NULL`, e.ID); err != nil {
		return fmt.Errorf("marcar publicado el evento %s: %w", e.ID, err)
	}
	return nil
}

// deliver anota el evento en el inbox del consumidor y, si es la primera vez, lo procesa en la misma tx.
// Dos entregas a la vez: la segunda espera el INSERT de la primera y, si esta confirma, no inserta.
func deliver(ctx context.Context, tx postgres.Tx, s subscription, e StoredEvent) error {
	tag, err := tx.Exec(ctx, `INSERT INTO infra.inbox (consumer, event_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, s.consumer, e.ID)
	if err != nil {
		return fmt.Errorf("anotar en el inbox: %w", err)
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
		return StoredEvent{}, fmt.Errorf("leer el evento %s: %w", id, err)
	}
	if correlation != nil {
		e.CorrelationID = *correlation
	}
	e.Data = payload
	return e, nil
}
