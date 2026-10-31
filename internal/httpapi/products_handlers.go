package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/DrewGGM/aluna/internal/access"
	"github.com/DrewGGM/aluna/internal/approvals"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/products"
)

var createProduct = operation{method: http.MethodPost, path: "/v1/products", success: http.StatusCreated}

// Maker-checker request kind of a product version publication (FR-18, TH-12) and the permission to request
// and to approve it.
const (
	productPublishKind       = "products.publish"
	productPublishPermission = "products.versions.publish"
)

// ProductHandlers implements the contract's product operations (api/openapi.yaml).
type ProductHandlers struct {
	products  *products.Service
	approvals *approvals.Service
	commands  commands
	reader    postgres.Querier
}

// NewProductHandlers creates the product handlers and registers in a the publication request kind, which
// goes through maker-checker. keys stores the responses of the commands with Idempotency-Key; reader serves
// the reads outside a transaction.
func NewProductHandlers(p *products.Service, a *approvals.Service, uow *postgres.UnitOfWork, keys *idempotency.Store, reader postgres.Querier) *ProductHandlers {
	a.RegisterKind(productPublishKind, productPublishPermission, publishOnApproval(p))
	return &ProductHandlers{products: p, approvals: a, commands: commands{uow: uow, keys: keys}, reader: reader}
}

// CreateProduct creates a product of the token's entity.
func (h *ProductHandlers) CreateProduct(ctx context.Context, req gen.CreateProductRequestObject) (gen.CreateProductResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := h.commands.run(ctx, createProduct, func(tx postgres.Tx) (any, error) {
		created, err := h.products.CreateProduct(ctx, tx, p.LegalEntityID, req.Body.Code, req.Body.Name)
		if err != nil {
			return nil, err
		}
		return gen.Product{Id: [16]byte(created.ID), Code: created.Code, Name: created.Name, CreatedAt: created.CreatedAt}, nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// CreateProductVersion stores the next version of a product of the token's entity as a draft, once per
// Idempotency-Key.
func (h *ProductHandlers) CreateProductVersion(ctx context.Context, req gen.CreateProductVersionRequestObject) (gen.CreateProductVersionResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	id := ids.ID(req.Id)
	op := resourceOperation("products", id, "versions", http.StatusCreated)
	resp, err := h.commands.idempotent(ctx, p, op, req.Params.IdempotencyKey, req.Body, func(tx postgres.Tx) (any, error) {
		created, err := h.products.CreateVersion(ctx, tx, p.LegalEntityID, id, p.Subject, req.Body.Definition)
		if err != nil {
			return nil, err
		}
		return toProductVersionJSON(created), nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// GetProductVersion reads a version of a product of the token's entity; another entity's version does not
// exist for it.
func (h *ProductHandlers) GetProductVersion(ctx context.Context, req gen.GetProductVersionRequestObject) (gen.GetProductVersionResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.products.Version(ctx, h.reader, ids.ID(req.Id))
	if err != nil {
		return nil, err
	}
	if v.LegalEntityID != p.LegalEntityID {
		return nil, fmt.Errorf("%w: %s", products.ErrVersionNotFound, v.ID)
	}
	return gen.GetProductVersion200JSONResponse(toProductVersionJSON(v)), nil
}

// publicationRequest is the payload of a publication request: the version the checker approves.
type publicationRequest struct {
	VersionID ids.ID `json:"version_id"`
	ProductID ids.ID `json:"product_id"`
	Version   int    `json:"version"`
}

// RequestProductPublication marks a version of the token's entity as pending and asks for its publication,
// which only its author does: it waits as a products.publish request until someone else approves it (FR-18,
// TH-12), once per Idempotency-Key.
func (h *ProductHandlers) RequestProductPublication(ctx context.Context, req gen.RequestProductPublicationRequestObject) (gen.RequestProductPublicationResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	id := ids.ID(req.Id)
	op := resourceOperation("product-versions", id, "publication", http.StatusAccepted)
	resp, err := h.commands.idempotent(ctx, p, op, req.Params.IdempotencyKey, nil, func(tx postgres.Tx) (any, error) {
		v, err := h.products.RequestPublication(ctx, tx, p.LegalEntityID, id, p.Subject)
		if err != nil {
			return nil, err
		}
		requested, err := h.approvals.Request(ctx, tx, productPublishKind, publicationRequest{VersionID: v.ID, ProductID: v.ProductID, Version: v.Number}, p)
		if err != nil {
			return nil, err
		}
		return toApprovalJSON(requested), nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// publicationResult is what an approved publication returns: the version in force, without its definition,
// which is read with products.versions.read.
type publicationResult struct {
	VersionID ids.ID          `json:"version_id"`
	ProductID ids.ID          `json:"product_id"`
	Version   int             `json:"version"`
	Status    products.Status `json:"status"`
}

// publishOnApproval is the Executor of productPublishKind: it publishes the version in the approver's entity. If
// the version is no longer pending or a later one is in force, the request is left failed.
func publishOnApproval(svc *products.Service) approvals.Executor {
	return func(ctx context.Context, tx postgres.Tx, payload json.RawMessage, checker access.Principal) (json.RawMessage, error) {
		var r publicationRequest
		if err := json.Unmarshal(payload, &r); err != nil {
			return nil, fmt.Errorf("publication payload: %w", err)
		}
		v, err := svc.Publish(ctx, tx, checker.LegalEntityID, r.VersionID)
		if err != nil {
			return nil, asCommandRejection(err)
		}
		result, err := json.Marshal(publicationResult{VersionID: v.ID, ProductID: v.ProductID, Version: v.Number, Status: v.Status})
		if err != nil {
			return nil, fmt.Errorf("publication result: %w", err)
		}
		return result, nil
	}
}

func toProductVersionJSON(v products.Version) gen.ProductVersion {
	out := gen.ProductVersion{
		Id: [16]byte(v.ID), ProductId: [16]byte(v.ProductID), Version: v.Number, Status: gen.ProductVersionStatus(v.Status),
		Definition: v.Document, CreatedBy: v.CreatedBy, CreatedAt: v.CreatedAt,
	}
	if !v.PublishedAt.IsZero() {
		out.PublishedAt = &v.PublishedAt
	}
	return out
}
