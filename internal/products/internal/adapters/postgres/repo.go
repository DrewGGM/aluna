// Package postgres stores products in products.products and their versions in products.versions.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/products/internal/app"
	"github.com/DrewGGM/aluna/internal/products/internal/domain"
)

// SQLSTATE of unique_violation and the constraint that keeps one code per entity.
const (
	uniqueViolation = "23505"
	uniqueCode      = "products_code_unique"
)

// Store implements the ports of app over a connection or a transaction.
type Store struct{ q postgres.Querier }

var (
	_ app.Products  = Store{}
	_ app.Drafts    = Store{}
	_ app.Lifecycle = Store{}
)

// NewStore creates the store over q, the pool or a Tx.
func NewStore(q postgres.Querier) Store { return Store{q: q} }

// InsertProduct stores p; the database sets created_at. A repeated code is app.ErrProductExists.
func (s Store) InsertProduct(ctx context.Context, p app.Product) (app.Product, error) {
	err := s.q.QueryRow(ctx, `INSERT INTO products.products (id, legal_entity_id, code, name) VALUES ($1, $2, $3, $4)
		RETURNING created_at`, p.ID, p.LegalEntityID, p.Code, p.Name).Scan(&p.CreatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == uniqueCode {
		return app.Product{}, fmt.Errorf("%w: %s", app.ErrProductExists, p.Code)
	}
	if err != nil {
		return app.Product{}, fmt.Errorf("insert product %s: %w", p.Code, err)
	}
	return p, nil
}

// Product returns product id of the entity, or app.ErrProductNotFound.
func (s Store) Product(ctx context.Context, entity, id ids.ID) (app.Product, error) {
	var p app.Product
	err := s.q.QueryRow(ctx, `SELECT id, legal_entity_id, code, name, created_at FROM products.products
		WHERE id = $1 AND legal_entity_id = $2`, id, entity).Scan(&p.ID, &p.LegalEntityID, &p.Code, &p.Name, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Product{}, fmt.Errorf("%w: %s", app.ErrProductNotFound, id)
	}
	if err != nil {
		return app.Product{}, fmt.Errorf("read product %s: %w", id, err)
	}
	return p, nil
}

// Lock takes an advisory lock on the product until the transaction ends: aluna_app cannot lock products
// FOR UPDATE (it has no UPDATE privilege on them, and must not).
func (s Store) Lock(ctx context.Context, productID ids.ID) error {
	if _, err := s.q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('products:' || $1::text, 0))`, productID); err != nil {
		return fmt.Errorf("lock product %s: %w", productID, err)
	}
	return nil
}

// LastNumber is the number of the product's latest version, or 0 if it has none.
func (s Store) LastNumber(ctx context.Context, productID ids.ID) (int, error) {
	var n int
	if err := s.q.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM products.versions WHERE product_id = $1`, productID).Scan(&n); err != nil {
		return 0, fmt.Errorf("last version of product %s: %w", productID, err)
	}
	return n, nil
}

// InsertVersion stores v as a draft; the database sets created_at, and v.Document becomes the stored
// document, as every later read gives it.
func (s Store) InsertVersion(ctx context.Context, v app.Version) (app.Version, error) {
	err := s.q.QueryRow(ctx, `INSERT INTO products.versions (id, product_id, version, definition, created_by)
		VALUES ($1, $2, $3, $4, $5) RETURNING definition, created_at`,
		v.ID, v.ProductID, v.Number, string(v.Document), v.CreatedBy).Scan(&v.Document, &v.CreatedAt)
	if err != nil {
		return app.Version{}, fmt.Errorf("insert version %d of product %s: %w", v.Number, v.ProductID, err)
	}
	return v, nil
}

const selectVersion = `SELECT v.id, v.product_id, p.legal_entity_id, v.version, v.status, v.definition, v.created_by,
		v.created_at, v.published_at
	FROM products.versions v JOIN products.products p ON p.id = v.product_id`

// Version returns version id, of any entity, or app.ErrVersionNotFound.
func (s Store) Version(ctx context.Context, id ids.ID) (app.Version, error) {
	v, err := scanVersion(s.q.QueryRow(ctx, selectVersion+` WHERE v.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Version{}, fmt.Errorf("%w: %s", app.ErrVersionNotFound, id)
	}
	if err != nil {
		return app.Version{}, fmt.Errorf("read product version %s: %w", id, err)
	}
	return v, nil
}

// Current returns the published version of the product, or app.ErrNoPublishedVersion.
func (s Store) Current(ctx context.Context, productID ids.ID) (app.Version, error) {
	v, err := scanVersion(s.q.QueryRow(ctx, selectVersion+` WHERE v.product_id = $1 AND v.status = 'published'`, productID))
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Version{}, fmt.Errorf("%w: product %s", app.ErrNoPublishedVersion, productID)
	}
	if err != nil {
		return app.Version{}, fmt.Errorf("read the published version of product %s: %w", productID, err)
	}
	return v, nil
}

// SetStatus moves version id to status to; publishing it sets published_at. The database checks the
// transition.
func (s Store) SetStatus(ctx context.Context, id ids.ID, to app.Status) (app.Version, error) {
	_, err := s.q.Exec(ctx, `UPDATE products.versions
		SET status = $2, published_at = CASE WHEN $2 = 'published' THEN now() ELSE published_at END WHERE id = $1`, id, to)
	if err != nil {
		return app.Version{}, fmt.Errorf("move product version %s to %s: %w", id, to, err)
	}
	return s.Version(ctx, id)
}

// errStoredDefinition means a stored definition the parser no longer reads: a fault of the data or of a
// stricter parser, not of the request, so it does not wrap the parser's sentinel (that would be a 422, and
// an approval would be left failed instead of pending).
var errStoredDefinition = errors.New("stored product definition does not parse")

// scanVersion reads a row of selectVersion; the stored definition is read again with the parser, so a
// version reads as the domain sees it.
func scanVersion(row pgx.Row) (app.Version, error) {
	var v app.Version
	var document []byte
	var publishedAt *time.Time
	err := row.Scan(&v.ID, &v.ProductID, &v.LegalEntityID, &v.Number, &v.Status, &document, &v.CreatedBy, &v.CreatedAt, &publishedAt)
	if err != nil {
		return app.Version{}, err
	}
	if v.Definition, err = domain.ParseDepositProduct(document); err != nil {
		return app.Version{}, fmt.Errorf("%w: version %s: %s", errStoredDefinition, v.ID, err.Error())
	}
	if publishedAt != nil {
		v.PublishedAt = *publishedAt
	}
	v.Document = document
	return v, nil
}
