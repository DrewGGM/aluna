// Package app holds the life cycle of products and their versions (FR-18): a version is created as a
// draft, its publication is requested (pending) and, when someone else approves it, it is published and the
// previous one retires.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/products/internal/domain"
)

// Status is the status of a product version.
type Status string

// Statuses of a product version.
const (
	Draft     Status = "draft"
	Pending   Status = "pending"
	Published Status = "published"
	Retired   Status = "retired"
)

var (
	// ErrInvalidProduct means a product code or name that is not valid.
	ErrInvalidProduct = errors.New("invalid product")
	// ErrProductExists means the entity already has a product with that code.
	ErrProductExists = errors.New("the entity already has a product with that code")
	// ErrProductNotFound means the product does not exist in the entity.
	ErrProductNotFound = errors.New("product not found")
	// ErrVersionNotFound means the product version does not exist in the entity.
	ErrVersionNotFound = errors.New("product version not found")
	// ErrNotPublishable means the version was already published or retired.
	ErrNotPublishable = errors.New("the product version is already published or retired")
	// ErrNotTheAuthor means someone other than the author of the version asked for its publication.
	ErrNotTheAuthor = errors.New("only the author of a product version asks for its publication")
	// ErrNotPending means the version has no publication requested, or it was already published.
	ErrNotPending = errors.New("the product version is not pending publication")
	// ErrSuperseded means a later version of the product is already published.
	ErrSuperseded = errors.New("a later version of the product is already published")
	// ErrNoPublishedVersion means the product has no version in force.
	ErrNoPublishedVersion = errors.New("the product has no published version")
)

const maxName = 200

var codePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Product is a family of versions of the entity, known by its code.
type Product struct {
	ID, LegalEntityID ids.ID
	Code, Name        string
	CreatedAt         time.Time
}

// Version is a version of a product. Document is the stored definition, as jsonb gives it back (its keys
// reordered); Definition is how the parser reads it. CreatedBy is the subject of its author; PublishedAt is
// zero until it is published.
type Version struct {
	ID, ProductID, LegalEntityID ids.ID
	Number                       int
	Status                       Status
	Definition                   domain.DepositProduct
	Document                     json.RawMessage
	CreatedBy                    string
	CreatedAt, PublishedAt       time.Time
}

// The ports of the use cases, over the transaction they run in. Lock serializes, until the transaction
// ends, the changes to the versions of a product; Version and Current read any entity's version, and the use
// cases check the entity.
type (
	// Products stores products.
	Products interface {
		InsertProduct(ctx context.Context, p Product) (Product, error)
	}
	// Drafts stores new versions.
	Drafts interface {
		Product(ctx context.Context, entity, id ids.ID) (Product, error)
		Lock(ctx context.Context, productID ids.ID) error
		LastNumber(ctx context.Context, productID ids.ID) (int, error)
		InsertVersion(ctx context.Context, v Version) (Version, error)
	}
	// Lifecycle moves versions through their statuses.
	Lifecycle interface {
		Lock(ctx context.Context, productID ids.ID) error
		Version(ctx context.Context, id ids.ID) (Version, error)
		Current(ctx context.Context, productID ids.ID) (Version, error)
		SetStatus(ctx context.Context, id ids.ID, to Status) (Version, error)
	}
)

// CreateProduct stores product p, or ErrInvalidProduct or ErrProductExists.
func CreateProduct(ctx context.Context, s Products, p Product) (Product, error) {
	if !codePattern.MatchString(p.Code) {
		return Product{}, fmt.Errorf("%w: the code is lowercase letters, digits and _, from 1 to 64", ErrInvalidProduct)
	}
	if n := utf8.RuneCountInString(p.Name); n < 1 || n > maxName {
		return Product{}, fmt.Errorf("%w: the name has from 1 to %d characters", ErrInvalidProduct, maxName)
	}
	return s.InsertProduct(ctx, p)
}

// CreateVersion stores, as a draft written by author, the next version of product productID of the entity
// with definition document, or domain.ErrInvalidProduct or ErrProductNotFound.
func CreateVersion(ctx context.Context, s Drafts, entity, productID ids.ID, author string, document []byte) (Version, error) {
	if _, err := s.Product(ctx, entity, productID); err != nil {
		return Version{}, err
	}
	definition, err := domain.ParseDepositProduct(document)
	if err != nil {
		return Version{}, err
	}
	if err := s.Lock(ctx, productID); err != nil {
		return Version{}, err
	}
	last, err := s.LastNumber(ctx, productID)
	if err != nil {
		return Version{}, err
	}
	return s.InsertVersion(ctx, Version{
		ID: ids.New(), ProductID: productID, LegalEntityID: entity, Number: last + 1, Status: Draft,
		Definition: definition, Document: document, CreatedBy: author,
	})
}

// RequestPublication marks version id of the entity as pending publication, asked for by its author:
// approvals then keeps whoever approves it from being the one who wrote its conditions. Requesting it again
// while pending leaves it so (a rejected request can be asked for again). Someone else is ErrNotTheAuthor;
// a published or retired version is ErrNotPublishable.
func RequestPublication(ctx context.Context, s Lifecycle, entity, id ids.ID, requester string) (Version, error) {
	v, err := lockedVersion(ctx, s, entity, id)
	switch {
	case err != nil:
		return Version{}, err
	case v.CreatedBy != requester:
		return Version{}, fmt.Errorf("%w: version %d", ErrNotTheAuthor, v.Number)
	case v.Status == Pending:
		return v, nil
	case v.Status != Draft:
		return Version{}, fmt.Errorf("%w: version %d is %s", ErrNotPublishable, v.Number, v.Status)
	}
	return s.SetStatus(ctx, id, Pending)
}

// Publish publishes version id of the entity, pending publication, and retires the one in force. An older
// version than the one in force is ErrSuperseded: publishing it would replace newer conditions.
func Publish(ctx context.Context, s Lifecycle, entity, id ids.ID) (Version, error) {
	v, err := lockedVersion(ctx, s, entity, id)
	if err != nil {
		return Version{}, err
	}
	if v.Status != Pending {
		return Version{}, fmt.Errorf("%w: version %d is %s", ErrNotPending, v.Number, v.Status)
	}
	current, err := s.Current(ctx, v.ProductID)
	switch {
	case errors.Is(err, ErrNoPublishedVersion): // none in force: nothing to retire
	case err != nil:
		return Version{}, err
	case current.Number > v.Number:
		return Version{}, fmt.Errorf("%w: version %d is in force", ErrSuperseded, current.Number)
	default:
		if _, err := s.SetStatus(ctx, current.ID, Retired); err != nil {
			return Version{}, err
		}
	}
	return s.SetStatus(ctx, id, Published)
}

// lockedVersion reads version id of the entity under the lock of its product, so its status is the latest.
func lockedVersion(ctx context.Context, s Lifecycle, entity, id ids.ID) (Version, error) {
	v, err := s.Version(ctx, id)
	if err != nil {
		return Version{}, err
	}
	if v.LegalEntityID != entity {
		return Version{}, fmt.Errorf("%w: %s", ErrVersionNotFound, id)
	}
	if err := s.Lock(ctx, v.ProductID); err != nil {
		return Version{}, err
	}
	return s.Version(ctx, id)
}
