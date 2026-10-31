//go:build integration

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// productWorld is an entity with two operators, A (the maker) and B, and a product created by A.
type productWorld struct {
	a, b    *httptest.Server
	product string
}

func newProductWorld(t *testing.T) productWorld {
	t.Helper()
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	entity := ids.New()
	w := productWorld{a: apiServerWith(t, entity, events), b: apiServerAs(t, entity, events, "operator-b")}
	var created gen.Product
	if status := call(t, w.a, http.MethodPost, "/v1/products", `{"code": "savings", "name": "Savings account"}`, &created); status != http.StatusCreated {
		t.Fatalf("create product = %d", status)
	}
	w.product = created.Id.String()
	return w
}

// createVersion creates, as A, a version with the definition of a domain testdata file.
func (w productWorld) createVersion(t *testing.T, file string) response {
	t.Helper()
	definition, err := os.ReadFile(filepath.Join("..", "products", "internal", "domain", "testdata", file)) //nolint:gosec // the test's own fixtures
	if err != nil {
		t.Fatal(err)
	}
	return postTo(t, w.a, "/v1/products/"+w.product+"/versions", ids.New().String(), `{"definition": `+string(definition)+`}`)
}

// requestPublication asks, as A, for the publication of version id and returns the request, which must
// be pending (202).
func (w productWorld) requestPublication(t *testing.T, id string) testApproval {
	t.Helper()
	r := postTo(t, w.a, "/v1/product-versions/"+id+"/publication", ids.New().String(), "")
	if r.status != http.StatusAccepted {
		t.Fatalf("request publication = %d %s, want 202", r.status, r.body)
	}
	return decodeApproval(t, r)
}

// readVersion reads version id as A; it must exist.
func (w productWorld) readVersion(t *testing.T, id string) gen.ProductVersion {
	t.Helper()
	var v gen.ProductVersion
	if status := call(t, w.a, http.MethodGet, "/v1/product-versions/"+id, "", &v); status != http.StatusOK {
		t.Fatalf("read version %s = %d", id, status)
	}
	return v
}

// createdVersion creates the version as createVersion and returns it, which must be created (201).
func (w productWorld) createdVersion(t *testing.T, file string) gen.ProductVersion {
	t.Helper()
	r := w.createVersion(t, file)
	var v gen.ProductVersion
	if err := json.Unmarshal(r.body, &v); err != nil || r.status != http.StatusCreated {
		t.Fatalf("create version = %d %s (%v), want 201", r.status, r.body, err)
	}
	return v
}

// TestAPublicationNeedsAnotherPerson (FR-18, US-4): A creates a version and asks for its publication (202,
// the version pending); A cannot approve it; B approves it and the version is published, with its
// definition.
func TestAPublicationNeedsAnotherPerson(t *testing.T) {
	w := newProductWorld(t)
	v := w.createdVersion(t, "valid-savings.json")
	request := w.requestPublication(t, v.Id.String())
	pending := w.readVersion(t, v.Id.String())
	path := "/v1/approvals/" + request.ID + "/approval"

	self := postTo(t, w.a, path, ids.New().String(), "")
	approved := postTo(t, w.b, path, ids.New().String(), "")
	got := w.readVersion(t, v.Id.String())

	if v.Status != gen.ProductVersionStatusDraft || v.Version != 1 || request.Kind != productPublishKind || pending.Status != gen.ProductVersionStatusPending {
		t.Fatalf("version = %+v, request = %+v, while pending = %+v", v, request, pending)
	}
	expectProblem(t, "A approves their own publication", self, http.StatusForbidden, "self_approval_forbidden")
	if decoded := decodeApproval(t, approved); approved.status != http.StatusOK || decoded.Status != "approved" {
		t.Fatalf("B approves = %d %s", approved.status, approved.body)
	}
	expectPublishedSavings(t, got)
}

// expectPublishedSavings checks that v is published with the definition of valid-savings.json.
func expectPublishedSavings(t *testing.T, v gen.ProductVersion) {
	t.Helper()
	var definition map[string]any
	if err := json.Unmarshal(v.Definition, &definition); err != nil || v.Status != gen.ProductVersionStatusPublished ||
		v.PublishedAt == nil || definition["dormancy_days"] != 365.0 {
		t.Fatalf("version = %+v (%v), want it published with its definition", v, err)
	}
}

// TestAProductIsChecked: an invalid definition is 422 and a repeated code 409, and another entity sees
// neither the product nor its versions.
func TestAProductIsChecked(t *testing.T) {
	w := newProductWorld(t)
	other := apiServer(t, ids.New())

	invalid := w.createVersion(t, "invalid-tiers-overlap.json")
	repeated := postTo(t, w.a, "/v1/products", ids.New().String(), `{"code": "savings", "name": "Again"}`)
	foreign := postTo(t, other, "/v1/products/"+w.product+"/versions", ids.New().String(), `{"definition": {}}`)
	v := w.createdVersion(t, "valid-savings.json")
	unseen := call(t, other, http.MethodGet, "/v1/product-versions/"+v.Id.String(), "", nil)

	expectProblem(t, "invalid definition", invalid, http.StatusUnprocessableEntity, "invalid_product_definition")
	expectProblem(t, "repeated code", repeated, http.StatusConflict, "product_already_exists")
	expectProblem(t, "another entity's product", foreign, http.StatusNotFound, "not_found")
	if unseen != http.StatusNotFound {
		t.Fatalf("another entity reads the version = %d, want 404", unseen)
	}
}

// TestOnlyTheAuthorAsksForThePublication (TH-12): B cannot ask for the publication of A's version, so A
// cannot approve it; the version shows A as its author.
func TestOnlyTheAuthorAsksForThePublication(t *testing.T) {
	w := newProductWorld(t)
	v := w.createdVersion(t, "valid-savings.json")

	byB := postTo(t, w.b, "/v1/product-versions/"+v.Id.String()+"/publication", ids.New().String(), "")

	expectProblem(t, "B asks for A's version", byB, http.StatusForbidden, "not_version_author")
	if got := w.readVersion(t, v.Id.String()); got.CreatedBy != testSubject || got.Status != gen.ProductVersionStatusDraft {
		t.Fatalf("version = %+v, want the draft by %s", got, testSubject)
	}
}

// TestAnUnreadableStoredVersionIs500: a stored definition the parser no longer reads is the server's fault,
// not a 422 for the client.
func TestAnUnreadableStoredVersionIs500(t *testing.T) {
	w := newProductWorld(t)
	v := w.createdVersion(t, "valid-savings.json")
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE products.versions SET definition = '{}' WHERE id = $1`, ids.ID(v.Id))
		return err
	})

	status := call(t, w.a, http.MethodGet, "/v1/product-versions/"+v.Id.String(), "", nil)

	if status != http.StatusInternalServerError {
		t.Fatalf("read = %d, want 500", status)
	}
}

// TestAPublishedVersionIsNotRequestedAgain: once published, asking for its publication is 409
// version_not_publishable.
func TestAPublishedVersionIsNotRequestedAgain(t *testing.T) {
	w := newProductWorld(t)
	v := w.createdVersion(t, "valid-savings.json")
	request := w.requestPublication(t, v.Id.String())
	if r := postTo(t, w.b, "/v1/approvals/"+request.ID+"/approval", ids.New().String(), ""); r.status != http.StatusOK {
		t.Fatalf("approve = %d %s", r.status, r.body)
	}

	again := postTo(t, w.a, "/v1/product-versions/"+v.Id.String()+"/publication", ids.New().String(), "")

	expectProblem(t, "request a published version", again, http.StatusConflict, "version_not_publishable")
}

// TestAStalePublicationFails: two versions are pending; once the newer one is published, approving the
// older one leaves its request failed and the newer one in force.
func TestAStalePublicationFails(t *testing.T) {
	w := newProductWorld(t)
	older := w.createdVersion(t, "valid-savings.json")
	newer := w.createdVersion(t, "valid-tiered-savings.json")
	olderRequest, newerRequest := w.requestPublication(t, older.Id.String()), w.requestPublication(t, newer.Id.String())
	if r := postTo(t, w.b, "/v1/approvals/"+newerRequest.ID+"/approval", ids.New().String(), ""); r.status != http.StatusOK {
		t.Fatalf("approve the newer = %d %s", r.status, r.body)
	}

	stale := postTo(t, w.b, "/v1/approvals/"+olderRequest.ID+"/approval", ids.New().String(), "")

	if got := decodeApproval(t, stale); stale.status != http.StatusOK || got.Status != "failed" {
		t.Fatalf("approve the older = %d %s, want 200 failed", stale.status, stale.body)
	}
	if v := w.readVersion(t, older.Id.String()); v.Status != gen.ProductVersionStatusPending {
		t.Fatalf("older version = %+v, want it still pending", v)
	}
}
