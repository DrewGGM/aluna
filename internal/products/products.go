// Package products is the product directory (ADR-0008): products, their versions defined by
// deposit-product.v1.json and their publication through maker-checker (FR-18). A published version is
// immutable; accounts are anchored to the version they were opened with (FR-19).
package products

import (
	"context"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres"
	pg "github.com/DrewGGM/aluna/internal/products/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/products/internal/app"
	"github.com/DrewGGM/aluna/internal/products/internal/domain"
)

type (
	// Product is a family of versions of the entity, known by its code.
	Product = app.Product
	// Version is a version of a product, with its definition.
	Version = app.Version
	// Status is the status of a product version.
	Status = app.Status
	// DepositProduct is the definition of a version of a deposit product.
	DepositProduct = domain.DepositProduct
)

// Statuses of a product version.
const (
	Draft     = app.Draft
	Pending   = app.Pending
	Published = app.Published
	Retired   = app.Retired
)

var (
	// ErrInvalidDefinition means a definition that does not follow deposit-product.v1.json.
	ErrInvalidDefinition = domain.ErrInvalidProduct
	// ErrInvalidProduct means a product code or name that is not valid.
	ErrInvalidProduct = app.ErrInvalidProduct
	// ErrProductExists means the entity already has a product with that code.
	ErrProductExists = app.ErrProductExists
	// ErrProductNotFound means the product does not exist in the entity.
	ErrProductNotFound = app.ErrProductNotFound
	// ErrVersionNotFound means the product version does not exist (in the entity, for the commands).
	ErrVersionNotFound = app.ErrVersionNotFound
	// ErrNotPublishable means the version was already published or retired.
	ErrNotPublishable = app.ErrNotPublishable
	// ErrNotTheAuthor means someone other than the author of the version asked for its publication.
	ErrNotTheAuthor = app.ErrNotTheAuthor
	// ErrNotPending means the version is not pending publication.
	ErrNotPending = app.ErrNotPending
	// ErrSuperseded means a later version of the product is already published.
	ErrSuperseded = app.ErrSuperseded
	// ErrNoPublishedVersion means the product has no version in force.
	ErrNoPublishedVersion = app.ErrNoPublishedVersion
)

// typePublished is the event of a published version (architecture §8).
const typePublished = "aluna.products.version.published.v1"

// Events publishes product events; outbox.Outbox implements it.
type Events interface {
	Emit(ctx context.Context, tx postgres.Tx, e outbox.Event) error
}

// Service manages products and their versions.
type Service struct {
	events Events
}

// New creates the service; it publishes its events to events.
func New(events Events) *Service { return &Service{events: events} }

// CreateProduct stores, within tx, a product of the entity with code and name, or ErrInvalidProduct or
// ErrProductExists.
func (s *Service) CreateProduct(ctx context.Context, tx postgres.Tx, entity ids.ID, code, name string) (Product, error) {
	return app.CreateProduct(ctx, pg.NewStore(tx), Product{ID: ids.New(), LegalEntityID: entity, Code: code, Name: name})
}

// CreateVersion stores, within tx, the next version of product productID of the entity as a draft written
// by author (a subject) with definition document, or ErrInvalidDefinition or ErrProductNotFound.
func (s *Service) CreateVersion(ctx context.Context, tx postgres.Tx, entity, productID ids.ID, author string, document []byte) (Version, error) {
	return app.CreateVersion(ctx, pg.NewStore(tx), entity, productID, author, document)
}

// RequestPublication marks, within tx, version id of the entity as pending publication, asked for by
// requester, its author; or ErrVersionNotFound, ErrNotTheAuthor or ErrNotPublishable. From then on its
// definition cannot change.
func (s *Service) RequestPublication(ctx context.Context, tx postgres.Tx, entity, id ids.ID, requester string) (Version, error) {
	return app.RequestPublication(ctx, pg.NewStore(tx), entity, id, requester)
}

// Publish publishes, within tx, version id of the entity, retires the one in force and emits
// aluna.products.version.published.v1; or ErrVersionNotFound, ErrNotPending or ErrSuperseded.
func (s *Service) Publish(ctx context.Context, tx postgres.Tx, entity, id ids.ID) (Version, error) {
	v, err := app.Publish(ctx, pg.NewStore(tx), entity, id)
	if err != nil {
		return Version{}, err
	}
	err = s.events.Emit(ctx, tx, outbox.Event{
		Type: typePublished, Subject: "product-versions/" + v.ID.String(), LegalEntityID: v.LegalEntityID, SchemaVersion: 1,
		Data: publishedPayload{ProductID: v.ProductID, VersionID: v.ID, Version: v.Number},
	})
	if err != nil {
		return Version{}, err
	}
	return v, nil
}

type publishedPayload struct {
	ProductID ids.ID `json:"product_id"`
	VersionID ids.ID `json:"version_id"`
	Version   int    `json:"version"`
}

// Version returns version versionID, whatever its status, or ErrVersionNotFound: an account reads the
// version it is anchored to. q can be the pool or a Tx.
func (s *Service) Version(ctx context.Context, q postgres.Querier, versionID ids.ID) (Version, error) {
	return pg.NewStore(q).Version(ctx, versionID)
}

// CurrentVersion returns the published version of product productID, the one new accounts are opened with,
// or ErrNoPublishedVersion. q can be the pool or a Tx.
func (s *Service) CurrentVersion(ctx context.Context, q postgres.Querier, productID ids.ID) (Version, error) {
	return pg.NewStore(q).Current(ctx, productID)
}
