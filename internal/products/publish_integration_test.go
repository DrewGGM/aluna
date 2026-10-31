//go:build integration

package products_test

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
	"github.com/DrewGGM/aluna/internal/products"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, &testPool)) }

// recorder keeps the emitted events instead of publishing them.
type recorder struct{ events []outbox.Event }

func (r *recorder) Emit(_ context.Context, _ postgres.Tx, e outbox.Event) error {
	r.events = append(r.events, e)
	return nil
}

// definition is a valid definition from the domain's testdata.
func definition(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("internal", "domain", "testdata", name)) //nolint:gosec // the test's own fixtures
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// author writes every version of the tests and asks for their publication.
const author = "product-designer"

// world is a product of an entity, with the service that manages it.
type world struct {
	svc     *products.Service
	events  *recorder
	entity  ids.ID
	product products.Product
}

func newWorld(t *testing.T) world {
	t.Helper()
	w := world{events: &recorder{}, entity: ids.New()}
	w.svc = products.New(w.events)
	inTx(t, func(tx postgres.Tx) error {
		var err error
		w.product, err = w.svc.CreateProduct(t.Context(), tx, w.entity, "savings", "Savings account")
		return err
	})
	return w
}

// inTx runs fn in a transaction; an error stops the test.
func inTx(t *testing.T, fn func(tx postgres.Tx) error) {
	t.Helper()
	if err := postgres.NewUnitOfWork(testPool).Do(t.Context(), fn); err != nil {
		t.Fatal(err)
	}
}

// try runs fn in a transaction and returns its error.
func try(t *testing.T, fn func(tx postgres.Tx) error) error {
	t.Helper()
	return postgres.NewUnitOfWork(testPool).Do(t.Context(), fn)
}

func (w world) version(t *testing.T, file string) products.Version {
	t.Helper()
	var v products.Version
	inTx(t, func(tx postgres.Tx) error {
		var err error
		v, err = w.svc.CreateVersion(t.Context(), tx, w.entity, w.product.ID, author, definition(t, file))
		return err
	})
	return v
}

// publish requests the publication of v and publishes it, as the approval would.
func (w world) publish(t *testing.T, v products.Version) products.Version {
	t.Helper()
	var published products.Version
	inTx(t, func(tx postgres.Tx) error {
		if _, err := w.svc.RequestPublication(t.Context(), tx, w.entity, v.ID, author); err != nil {
			return err
		}
		var err error
		published, err = w.svc.Publish(t.Context(), tx, w.entity, v.ID)
		return err
	})
	return published
}

// TestAPublishedVersionKeepsItsDefinition (FR-18, FR-19): after publishing v1 and v2, v1 still reads its
// original definition, retired, and v2 is the version in force.
func TestAPublishedVersionKeepsItsDefinition(t *testing.T) {
	w := newWorld(t)
	draft := w.version(t, "valid-savings.json")
	w.publish(t, draft)
	v2 := w.publish(t, w.version(t, "valid-tiered-savings.json"))

	got, gotErr := w.svc.Version(t.Context(), testPool, draft.ID)
	current, currentErr := w.svc.CurrentVersion(t.Context(), testPool, w.product.ID)
	if err := cmp.Or(gotErr, currentErr); err != nil {
		t.Fatal(err)
	}

	// draft.Definition was read from the original document, before it was stored
	if fmt.Sprintf("%+v", got.Definition) != fmt.Sprintf("%+v", draft.Definition) || got.Status != products.Retired || got.Number != 1 || got.PublishedAt.IsZero() {
		t.Fatalf("v1 = %+v, want its original definition, retired", got)
	}
	if current.ID != v2.ID || current.Status != products.Published || current.Number != 2 || v2.PublishedAt.IsZero() {
		t.Fatalf("version in force = %+v, want v2 published", current)
	}
}

// TestAPublicationEmitsItsEvent: publishing a version emits aluna.products.version.published.v1 about it,
// and nothing else does.
func TestAPublicationEmitsItsEvent(t *testing.T) {
	w := newWorld(t)
	v := w.version(t, "valid-savings.json")
	inTx(t, func(tx postgres.Tx) error {
		_, err := w.svc.RequestPublication(t.Context(), tx, w.entity, v.ID, author)
		return err
	})
	if len(w.events.events) != 0 {
		t.Fatalf("events before publishing = %+v, want none", w.events.events)
	}

	w.publish(t, v)

	got := w.events.events
	if len(got) != 1 {
		t.Fatalf("events = %+v, want one", got)
	}
	data, err := json.Marshal(got[0].Data)
	wantData := `{"product_id":"` + w.product.ID.String() + `","version_id":"` + v.ID.String() + `","version":1}`
	if err != nil || got[0].Type != "aluna.products.version.published.v1" || got[0].Subject != "product-versions/"+v.ID.String() ||
		got[0].LegalEntityID != w.entity || got[0].SchemaVersion != 1 || string(data) != wantData {
		t.Fatalf("event = %+v with data %s, want the publication of version 1", got[0], data)
	}
}

// TestAVersionStartsAsADraft: a new version is the next number, a draft, with its definition read and its
// document kept; the product has no version in force until one is published.
func TestAVersionStartsAsADraft(t *testing.T) {
	w := newWorld(t)
	first, second := w.version(t, "valid-savings.json"), w.version(t, "valid-simplified-deposit.json")

	_, noCurrent := w.svc.CurrentVersion(t.Context(), testPool, w.product.ID)

	stored, err := w.svc.Version(t.Context(), testPool, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Number != 1 || second.Number != 2 || second.Status != products.Draft || second.Definition.DormancyDays != 365 ||
		string(second.Document) != string(stored.Document) || !second.PublishedAt.IsZero() {
		t.Fatalf("versions = %+v, %+v; want drafts 1 and 2", first, second)
	}
	if !errors.Is(noCurrent, products.ErrNoPublishedVersion) {
		t.Fatalf("version in force of a product without publications = %v, want ErrNoPublishedVersion", noCurrent)
	}
}

// TestTheLifeCycleIsFollowed: a draft is not published without its request, a published version is not
// requested again, and an older version does not replace a newer one in force.
func TestTheLifeCycleIsFollowed(t *testing.T) {
	w := newWorld(t)
	draft := w.version(t, "valid-savings.json")
	older, newer := w.version(t, "valid-savings.json"), w.version(t, "valid-tiered-savings.json")
	inTx(t, func(tx postgres.Tx) error {
		_, err := w.svc.RequestPublication(t.Context(), tx, w.entity, older.ID, author)
		return err
	})
	w.publish(t, newer)

	unrequested := try(t, func(tx postgres.Tx) error { _, err := w.svc.Publish(t.Context(), tx, w.entity, draft.ID); return err })
	again := try(t, func(tx postgres.Tx) error {
		_, err := w.svc.RequestPublication(t.Context(), tx, w.entity, newer.ID, author)
		return err
	})
	superseded := try(t, func(tx postgres.Tx) error { _, err := w.svc.Publish(t.Context(), tx, w.entity, older.ID); return err })

	if !errors.Is(unrequested, products.ErrNotPending) || !errors.Is(again, products.ErrNotPublishable) || !errors.Is(superseded, products.ErrSuperseded) {
		t.Fatalf("publish a draft = %v, request a published one = %v, publish an older one = %v", unrequested, again, superseded)
	}
	if current, err := w.svc.CurrentVersion(t.Context(), testPool, w.product.ID); err != nil || current.ID != newer.ID {
		t.Fatalf("version in force = %+v (%v), want the newer one", current, err)
	}
}

// TestOnlyTheAuthorRequestsThePublication (TH-12): someone other than the author of a version cannot ask
// for its publication, so they cannot get the author to approve it; the version records its author.
func TestOnlyTheAuthorRequestsThePublication(t *testing.T) {
	w := newWorld(t)
	v := w.version(t, "valid-savings.json")

	err := try(t, func(tx postgres.Tx) error {
		_, err := w.svc.RequestPublication(t.Context(), tx, w.entity, v.ID, "someone-else")
		return err
	})
	got, getErr := w.svc.Version(t.Context(), testPool, v.ID)

	if !errors.Is(err, products.ErrNotTheAuthor) || getErr != nil || got.Status != products.Draft || got.CreatedBy != author {
		t.Fatalf("another person asks = %v; version = %+v (%v), want ErrNotTheAuthor and the draft by %s", err, got, getErr, author)
	}
}

// TestAPublicationCanBeRequestedAgain: requesting a pending version again leaves it pending, so a rejected
// request can be asked for again.
func TestAPublicationCanBeRequestedAgain(t *testing.T) {
	w := newWorld(t)
	v := w.version(t, "valid-savings.json")
	var first, second products.Version
	inTx(t, func(tx postgres.Tx) error {
		var err error
		if first, err = w.svc.RequestPublication(t.Context(), tx, w.entity, v.ID, author); err != nil {
			return err
		}
		second, err = w.svc.RequestPublication(t.Context(), tx, w.entity, v.ID, author)
		return err
	})

	if first.Status != products.Pending || second.Status != products.Pending {
		t.Fatalf("requests = %s, %s; want pending both times", first.Status, second.Status)
	}
}

// TestAnotherEntitysProductDoesNotExist: an entity cannot add versions to, request or publish another
// entity's product.
func TestAnotherEntitysProductDoesNotExist(t *testing.T) {
	w := newWorld(t)
	v := w.version(t, "valid-savings.json")
	other := ids.New()

	create := try(t, func(tx postgres.Tx) error {
		_, err := w.svc.CreateVersion(t.Context(), tx, other, w.product.ID, author, definition(t, "valid-savings.json"))
		return err
	})
	request := try(t, func(tx postgres.Tx) error {
		_, err := w.svc.RequestPublication(t.Context(), tx, other, v.ID, author)
		return err
	})
	publish := try(t, func(tx postgres.Tx) error { _, err := w.svc.Publish(t.Context(), tx, other, v.ID); return err })

	if !errors.Is(create, products.ErrProductNotFound) || !errors.Is(request, products.ErrVersionNotFound) || !errors.Is(publish, products.ErrVersionNotFound) {
		t.Fatalf("another entity: create = %v, request = %v, publish = %v", create, request, publish)
	}
}

// TestProductsAreChecked: a repeated code, an invalid code or name and an invalid definition are rejected,
// and nothing is stored.
func TestProductsAreChecked(t *testing.T) {
	w := newWorld(t)
	create := func(code, name string) error {
		return try(t, func(tx postgres.Tx) error {
			_, err := w.svc.CreateProduct(t.Context(), tx, w.entity, code, name)
			return err
		})
	}

	repeated, badCode, noName := create("savings", "Another"), create("Savings", "Savings"), create("payroll", "")
	invalid := try(t, func(tx postgres.Tx) error {
		_, err := w.svc.CreateVersion(t.Context(), tx, w.entity, w.product.ID, author, []byte(`{"currency": "COP"}`))
		return err
	})

	if !errors.Is(repeated, products.ErrProductExists) || !errors.Is(badCode, products.ErrInvalidProduct) ||
		!errors.Is(noName, products.ErrInvalidProduct) || !errors.Is(invalid, products.ErrInvalidDefinition) {
		t.Fatalf("repeated = %v, bad code = %v, no name = %v, invalid definition = %v", repeated, badCode, noName, invalid)
	}
	if v := w.version(t, "valid-savings.json"); v.Number != 1 {
		t.Fatalf("first stored version = %d, the rejected one should not count", v.Number)
	}
}

// TestConcurrentPublicationsAreSerialized: while the publication of v1 is uncommitted, publishing v2 waits
// for it on the product's lock and then retires it, instead of both seeing no version in force.
func TestConcurrentPublicationsAreSerialized(t *testing.T) {
	w := newWorld(t)
	v1, v2 := w.version(t, "valid-savings.json"), w.version(t, "valid-tiered-savings.json")
	inTx(t, func(tx postgres.Tx) error {
		for _, v := range []products.Version{v1, v2} {
			if _, err := w.svc.RequestPublication(t.Context(), tx, w.entity, v.ID, author); err != nil {
				return err
			}
		}
		return nil
	})
	published, release, first := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce() // if the test fails early, the first transaction is not left open
	go func() {
		first <- postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
			if _, err := w.svc.Publish(t.Context(), tx, w.entity, v1.ID); err != nil {
				return err
			}
			close(published)
			<-release
			return nil
		})
	}()
	select {
	case <-published:
	case err := <-first:
		t.Fatalf("the first publication did not happen: %v", err)
	}
	second := make(chan error, 1)
	go func() {
		second <- try(t, func(tx postgres.Tx) error { _, err := w.svc.Publish(t.Context(), tx, w.entity, v2.ID); return err })
	}()
	waitForLockWait(t, "advisory")
	releaseOnce()

	if err := cmp.Or(<-first, <-second); err != nil {
		t.Fatal(err)
	}
	old, err := w.svc.Version(t.Context(), testPool, v1.ID)
	if current, currentErr := w.svc.CurrentVersion(t.Context(), testPool, w.product.ID); cmp.Or(err, currentErr) != nil ||
		current.ID != v2.ID || old.Status != products.Retired {
		t.Fatalf("version in force = %+v, v1 = %s (%v, %v); want v2 in force and v1 retired", current, old.Status, err, currentErr)
	}
}

// waitForLockWait waits, for less than lock_timeout (2 s), until a session waits for a lock of locktype
// held by a session of this test database. A second publication waits for the first on the product's
// "advisory" lock (without it, it would wait on the index of the version in force instead, and this
// fails); a second overlapping load waits for the first one's transaction ("transactionid") on the
// exclusion constraint.
func waitForLockWait(t *testing.T, locktype string) {
	t.Helper()
	for deadline := time.Now().Add(1500 * time.Millisecond); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		var waiting int
		err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM pg_locks w
			WHERE w.locktype = $1 AND NOT w.granted
				AND EXISTS (SELECT 1 FROM pg_locks l WHERE l.pid = w.pid
					AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database()))`, locktype).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
	}
	t.Fatalf("no session waited on a %s lock", locktype)
}

// TestAnUnreadableStoredDefinitionIsAnInternalError: a stored definition the parser no longer reads (here,
// a draft emptied by the tables' owner) is not the client's invalid definition: it would be a 422, and an
// approval would be left failed instead of pending.
func TestAnUnreadableStoredDefinitionIsAnInternalError(t *testing.T) {
	w := newWorld(t)
	v := w.version(t, "valid-savings.json")
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE products.versions SET definition = '{}' WHERE id = $1`, v.ID)
		return err
	})

	_, err := w.svc.Version(t.Context(), testPool, v.ID)

	if err == nil || errors.Is(err, products.ErrInvalidDefinition) {
		t.Fatalf("read = %v, want an error that is not ErrInvalidDefinition", err)
	}
}

// TestTheDatabaseKeepsVersionsImmutable: even as the tables' owner, the definition of a pending or
// published version does not change, a version is not deleted, its status does not go back, its number and
// author do not change and a product does not change.
func TestTheDatabaseKeepsVersionsImmutable(t *testing.T) {
	w := newWorld(t)
	published := w.publish(t, w.version(t, "valid-savings.json"))
	pending := w.version(t, "valid-tiered-savings.json")
	inTx(t, func(tx postgres.Tx) error {
		_, err := w.svc.RequestPublication(t.Context(), tx, w.entity, pending.ID, author)
		return err
	})
	cases := []struct{ name, sql, message string }{
		{"change a published definition", `UPDATE products.versions SET definition = '{}' WHERE id = '` + published.ID.String() + `'`, "is fixed once its publication is requested"},
		{"change a pending definition", `UPDATE products.versions SET definition = '{}' WHERE id = '` + pending.ID.String() + `'`, "is fixed once its publication is requested"},
		{"delete a version", `DELETE FROM products.versions WHERE id = '` + published.ID.String() + `'`, "are never deleted"},
		{"unpublish", `UPDATE products.versions SET status = 'pending' WHERE id = '` + published.ID.String() + `'`, "cannot go from published to pending"},
		{"change the author", `UPDATE products.versions SET created_by = 'other' WHERE id = '` + pending.ID.String() + `'`, "only the status"},
		{"renumber", `UPDATE products.versions SET version = 9 WHERE id = '` + pending.ID.String() + `'`, "only the status"},
		{"rename a product", `UPDATE products.products SET name = 'Other' WHERE id = '` + w.product.ID.String() + `'`, "products are never updated or deleted"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var err error
			pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
				// a savepoint, so the expected error does not abort the transaction AsOwner commits
				sp, beginErr := tx.Begin(t.Context())
				if beginErr != nil {
					return beginErr
				}
				_, err = sp.Exec(t.Context(), c.sql)
				return sp.Rollback(t.Context())
			})
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "2F003" || !strings.Contains(pgErr.Message, c.message) {
				t.Fatalf("got %v, want prohibited_sql_statement_attempted: %s", err, c.message)
			}
		})
	}
}
