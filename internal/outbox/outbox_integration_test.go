//go:build integration

package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, &testPool)) }

const (
	happened       = "aluna.test.thing.happened.v1"
	checkViolation = "23514"
)

var errFlaky = errors.New("falla a propósito")

// recorder cuenta cuántas veces se llamó su handler con cada evento; las primeras failFirst llamadas
// fallan.
type recorder struct {
	mu        sync.Mutex
	calls     map[ids.ID]int
	failFirst int
}

func newRecorder() *recorder { return &recorder{calls: map[ids.ID]int{}} }

func (r *recorder) handle(_ context.Context, _ postgres.Tx, e StoredEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[e.ID]++
	if r.calls[e.ID] <= r.failFirst {
		return errFlaky
	}
	return nil
}

func (r *recorder) count(id ids.ID) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[id]
}

func newOutbox(t *testing.T, rec *recorder) *Outbox {
	t.Helper()
	o, err := New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	o.Subscribe("test", []string{happened}, rec.handle)
	return o
}

// work arranca un cliente River que publica los eventos de o, como el worker (T-031), y devuelve los
// jobs que termina.
func work(t *testing.T, o *Outbox) <-chan *river.Event {
	t.Helper()
	workers := river.NewWorkers()
	o.AddWorkers(workers)
	client, err := river.NewClient(riverpgxv5.New(testPool), &river.Config{
		Schema: RiverSchema, Queues: map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 2}}, Workers: workers,
		FetchPollInterval: 100 * time.Millisecond, TestOnly: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	finished, _ := client.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
	if err := client.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Error(err)
		}
	})
	return finished
}

// waitFor espera a que termine el job que publica el evento id.
func waitFor(t *testing.T, finished <-chan *river.Event, id ids.ID) *river.Event {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev := <-finished:
			var args publishArgs
			if err := json.Unmarshal(ev.Job.EncodedArgs, &args); err != nil {
				t.Fatal(err)
			}
			if args.EventID == id {
				return ev
			}
		case <-timeout:
			t.Fatalf("el job del evento %s no terminó en 10 s", id)
		}
	}
}

func event() Event {
	return Event{
		ID: ids.New(), Type: happened, Subject: "cosa/" + ids.New().String(), LegalEntityID: ids.New(), CorrelationID: ids.New(),
		SchemaVersion: 1, Data: map[string]any{"units": 100},
	}
}

// emit emite e en su propia transacción. El job que queda se borra al final para que no lo trabaje el
// cliente River de otra prueba.
func emit(t *testing.T, o *Outbox, e Event) {
	t.Helper()
	err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error { return o.Emit(t.Context(), tx, e) })
	if err != nil {
		t.Fatal(err)
	}
	dropJob(t, e.ID)
}

// dropJob borra al final de la prueba el job que publica el evento id.
func dropJob(t *testing.T, id ids.ID) {
	t.Helper()
	t.Cleanup(func() {
		_, err := testPool.Exec(context.WithoutCancel(t.Context()), `DELETE FROM infra.river_job WHERE args->>'event_id' = $1::text`, id)
		if err != nil {
			t.Error(err)
		}
	})
}

func publish(t *testing.T, o *Outbox, id ids.ID) error {
	t.Helper()
	return (&publishWorker{o: o}).Work(t.Context(), &river.Job[publishArgs]{Args: publishArgs{EventID: id}})
}

func published(t *testing.T, id ids.ID) bool {
	t.Helper()
	var at *time.Time
	if err := testPool.QueryRow(t.Context(), `SELECT published_at FROM infra.outbox WHERE id = $1`, id).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at != nil
}

// TestACommittedEventIsDelivered: el job del evento confirmado lo entrega al consumidor y lo marca
// publicado.
func TestACommittedEventIsDelivered(t *testing.T) {
	rec := newRecorder()
	o := newOutbox(t, rec)
	finished := work(t, o)
	e := event()

	emit(t, o, e)

	if ev := waitFor(t, finished, e.ID); ev.Kind != river.EventKindJobCompleted {
		t.Fatalf("el job terminó en %s", ev.Kind)
	}
	if n := rec.count(e.ID); n != 1 || !published(t, e.ID) {
		t.Fatalf("entregas %d (publicado: %v), se esperaba 1 y publicado", n, published(t, e.ID))
	}
}

// TestARolledBackEventIsNeverDelivered: si la transacción que lo emite se revierte, no queda ni el
// evento ni el job que lo entregaría.
func TestARolledBackEventIsNeverDelivered(t *testing.T) {
	o := newOutbox(t, newRecorder())
	e := event()
	errRollback := errors.New("se revierte a propósito")

	err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		if err := o.Emit(t.Context(), tx, e); err != nil {
			return err
		}
		return errRollback
	})

	var events, jobs int
	if !errors.Is(err, errRollback) {
		t.Fatalf("emitir = %v, se esperaba el rollback", err)
	}
	err = testPool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM infra.outbox WHERE id = $1),
		(SELECT count(*) FROM infra.river_job WHERE args->>'event_id' = $1::text)`, e.ID).Scan(&events, &jobs)
	if err != nil || events != 0 || jobs != 0 {
		t.Fatalf("evento revertido: %d filas, %d jobs (%v); se esperaba nada", events, jobs, err)
	}
}

// TestADuplicateDeliveryRunsTheHandlerOnce: el job de publicación es at-least-once; corrido dos veces
// para el mismo evento, el consumidor lo procesa una vez (inbox).
func TestADuplicateDeliveryRunsTheHandlerOnce(t *testing.T) {
	rec := newRecorder()
	o := newOutbox(t, rec)
	e := event()
	emit(t, o, e)

	for range 2 {
		if err := publish(t, o, e.ID); err != nil {
			t.Fatal(err)
		}
	}

	if n := rec.count(e.ID); n != 1 || !published(t, e.ID) {
		t.Fatalf("el consumidor procesó el evento %d veces (publicado: %v), se esperaba 1", n, published(t, e.ID))
	}
}

// TestAFailingConsumerDoesNotBlockTheOthers: si un consumidor falla, los demás reciben el evento igual;
// el reintento solo se lo entrega al que falló, y el evento queda publicado cuando todos lo procesaron.
func TestAFailingConsumerDoesNotBlockTheOthers(t *testing.T) {
	first, flaky, last := newRecorder(), newRecorder(), newRecorder()
	flaky.failFirst = 1
	o := newOutbox(t, first)
	o.Subscribe("flaky", []string{happened}, flaky.handle)
	o.Subscribe("last", []string{happened}, last.handle)
	e := event()
	emit(t, o, e)

	if err := publish(t, o, e.ID); !errors.Is(err, errFlaky) || published(t, e.ID) {
		t.Fatalf("primer intento = %v (publicado: %v), se esperaba la falla y sin publicar", err, published(t, e.ID))
	}
	if last.count(e.ID) != 1 {
		t.Fatalf("tras la falla del anterior, el último consumidor recibió el evento %d veces, se esperaba 1", last.count(e.ID))
	}
	if err := publish(t, o, e.ID); err != nil {
		t.Fatal(err)
	}

	if first.count(e.ID) != 1 || flaky.count(e.ID) != 2 || last.count(e.ID) != 1 || !published(t, e.ID) {
		t.Fatalf("llamadas: primero %d, el que falla %d, último %d (publicado: %v); se esperaba 1, 2, 1 y publicado",
			first.count(e.ID), flaky.count(e.ID), last.count(e.ID), published(t, e.ID))
	}
}

// TestOnlySubscribersOfTheTypeReceiveIt: un consumidor no recibe tipos a los que no se suscribió.
func TestOnlySubscribersOfTheTypeReceiveIt(t *testing.T) {
	rec, other := newRecorder(), newRecorder()
	o := newOutbox(t, rec)
	o.Subscribe("otro", []string{"aluna.test.thing.vanished.v1"}, other.handle)
	e := event()
	emit(t, o, e)

	if err := publish(t, o, e.ID); err != nil {
		t.Fatal(err)
	}

	if rec.count(e.ID) != 1 || other.count(e.ID) != 0 {
		t.Fatalf("entregas: suscrito %d, otro tipo %d; se esperaba 1 y 0", rec.count(e.ID), other.count(e.ID))
	}
}

// TestEmitFillsAMissingIDAndOmitsAMissingCorrelation: sin ID, Emit le asigna uno; sin correlación, la
// columna queda NULL y el CloudEvent no trae correlationid.
func TestEmitFillsAMissingIDAndOmitsAMissingCorrelation(t *testing.T) {
	o := newOutbox(t, newRecorder())
	e := event()
	e.ID, e.CorrelationID = ids.ID{}, ids.ID{}
	err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error { return o.Emit(t.Context(), tx, e) })
	if err != nil {
		t.Fatal(err)
	}

	var id ids.ID
	var noCorrelation bool
	err = testPool.QueryRow(t.Context(), `SELECT id, correlation_id IS NULL FROM infra.outbox WHERE subject = $1`, e.Subject).Scan(&id, &noCorrelation)
	if err != nil || id == (ids.ID{}) || !noCorrelation {
		t.Fatalf("id %s, correlación NULL %v (%v); se esperaba un id y NULL", id, noCorrelation, err)
	}
	dropJob(t, id)
	stored, err := o.load(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["correlationid"]; ok {
		t.Fatalf("CloudEvent = %s, no debía traer correlationid", body)
	}
}

// TestAnEventWithoutLegalEntityIsRejected: todo evento pertenece a una entidad legal.
func TestAnEventWithoutLegalEntityIsRejected(t *testing.T) {
	o := newOutbox(t, newRecorder())
	e := event()
	e.LegalEntityID = ids.ID{}

	err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error { return o.Emit(t.Context(), tx, e) })

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != checkViolation {
		t.Fatalf("emitir sin entidad legal = %v, se esperaba la violación del CHECK", err)
	}
}

// TestThePgxTxCannotBeClosedOutsideTheUnitOfWork: lo que recibe River de Tx.Pgx no confirma ni revierte
// la transacción del comando.
func TestThePgxTxCannotBeClosedOutsideTheUnitOfWork(t *testing.T) {
	err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		return errors.Join(tx.Pgx().Commit(t.Context()), tx.Pgx().Rollback(t.Context()))
	})

	var failures interface{ Unwrap() []error }
	if !errors.As(err, &failures) || len(failures.Unwrap()) != 2 || !errors.Is(err, postgres.ErrSharedTx) {
		t.Fatalf("Commit y Rollback sobre Pgx() = %v, se esperaba ErrSharedTx en los dos", err)
	}
}

// TestAStoredEventIsACloudEvent: el envelope es CloudEvents 1.0 JSON con las extensiones legalentityid,
// correlationid y schemaversion.
func TestAStoredEventIsACloudEvent(t *testing.T) {
	e := StoredEvent{
		ID: ids.New(), Type: happened, Subject: "cosa/1", LegalEntityID: ids.New(), CorrelationID: ids.New(), SchemaVersion: 1,
		Data: json.RawMessage(`{"units":100}`), Time: time.Date(2026, time.October, 27, 15, 4, 5, 0, time.UTC),
	}

	body, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"specversion": "1.0", "id": e.ID.String(), "source": "aluna/test", "type": happened, "subject": "cosa/1",
		"time": "2026-10-27T15:04:05Z", "datacontenttype": "application/json", "data": map[string]any{"units": float64(100)},
		"legalentityid": e.LegalEntityID.String(), "correlationid": e.CorrelationID.String(), "schemaversion": float64(1),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CloudEvent = %s, se esperaba %v", body, want)
	}
}

// TestPurgeDeletesOnlyPublishedEvents: la purga se lleva los eventos publicados antes del corte y deja los
// que aún no se publican. El publicado tiene su fila en el inbox: sin el borrado en cascada, la purga
// fallaría por la llave foránea.
func TestPurgeDeletesOnlyPublishedEvents(t *testing.T) {
	o := newOutbox(t, newRecorder())
	done, pending := event(), event()
	emit(t, o, done)
	emit(t, o, pending)
	if err := publish(t, o, done.ID); err != nil {
		t.Fatal(err)
	}

	n, err := o.Purge(t.Context(), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	var left []ids.ID
	rows, err := testPool.Query(t.Context(), `SELECT id FROM infra.outbox WHERE id IN ($1, $2)`, done.ID, pending.ID)
	if err == nil {
		left, err = pgx.CollectRows(rows, pgx.RowTo[ids.ID])
	}
	if err != nil || n < 1 || !reflect.DeepEqual(left, []ids.ID{pending.ID}) {
		t.Fatalf("purga: %d borrados, quedan %v (%v); se esperaba solo %s", n, left, err, pending.ID)
	}
}

// TestPurgeDeletesMoreThanOneBatch: con más publicados viejos que un lote, la purga sigue hasta borrarlos
// todos.
func TestPurgeDeletesMoreThanOneBatch(t *testing.T) {
	o := newOutbox(t, newRecorder())
	prefix := "lote-" + ids.New().String() + "/"
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO infra.outbox (id, type, subject, legal_entity_id, schema_version, payload, published_at)
			SELECT gen_random_uuid(), $1, $2 || g, $3, 1, '{}', clock_timestamp() - interval '1 hour'
			FROM generate_series(1, $4::int) g`, happened, prefix, ids.New(), purgeBatch+1)
		return err
	})

	n, err := o.Purge(t.Context(), time.Now())

	var left int
	if err == nil {
		err = testPool.QueryRow(t.Context(), `SELECT count(*) FROM infra.outbox WHERE subject LIKE $1 || '%'`, prefix).Scan(&left)
	}
	if err != nil || n < purgeBatch+1 || left != 0 {
		t.Fatalf("purga de %d eventos: borró %d, quedan %d (%v)", purgeBatch+1, n, left, err)
	}
}
