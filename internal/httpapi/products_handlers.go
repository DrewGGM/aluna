package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/DrewGGM/aluna/internal/access"
	"github.com/DrewGGM/aluna/internal/approvals"
	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/products"
	openapi_types "github.com/oapi-codegen/runtime/types"
)

var (
	createProduct       = operation{method: http.MethodPost, path: "/v1/products", success: http.StatusCreated}
	loadMarketParameter = operation{method: http.MethodPost, path: "/v1/market-parameters", success: http.StatusAccepted}
)

// Maker-checker request kinds of a product version publication (FR-18, TH-12) and of a market parameter
// load (FR-21), and the permission to request and to approve each one.
const (
	productPublishKind       = "products.publish"
	productPublishPermission = "products.versions.publish"
	marketLoadKind           = "products.market_parameter_load"
	marketLoadPermission     = "products.market_parameters.load"
)

// ProductHandlers implements the contract's product operations (api/openapi.yaml).
type ProductHandlers struct {
	products  *products.Service
	approvals *approvals.Service
	commands  commands
	reader    postgres.Querier
}

// NewProductHandlers creates the product handlers and registers in a the request kinds of a product
// publication and of a market parameter load, which go through maker-checker. keys stores the responses of
// the commands with Idempotency-Key; reader serves the reads outside a transaction.
func NewProductHandlers(p *products.Service, a *approvals.Service, uow *postgres.UnitOfWork, keys *idempotency.Store, reader postgres.Querier) *ProductHandlers {
	a.RegisterKind(productPublishKind, productPublishPermission, publishOnApproval(p))
	a.RegisterKind(marketLoadKind, marketLoadPermission, loadMarketParameterOnApproval(p))
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

// marketLoadRequest is the payload of a market parameter load request: the value the checker approves.
type marketLoadRequest struct {
	Kind      products.MarketKind `json:"kind"`
	Modality  string              `json:"modality,omitempty"`
	ValidFrom openapi_types.Date  `json:"valid_from"`
	ValidTo   openapi_types.Date  `json:"valid_to"`
	Value     string              `json:"value"`
}

// parameter is the market parameter the request asks to load, or products.ErrInvalidMarketParameter. A
// missing date (the generated code does not check required fields) is not year 1: it is rejected.
func (r marketLoadRequest) parameter() (products.MarketParameter, error) {
	if r.ValidFrom.IsZero() || r.ValidTo.IsZero() {
		return products.MarketParameter{}, fmt.Errorf("%w: valid_from and valid_to are required", products.ErrInvalidMarketParameter)
	}
	value, err := products.ParseMarketValue(r.Value)
	if err != nil {
		return products.MarketParameter{}, err
	}
	return products.MarketParameter{
		Kind: r.Kind, Modality: r.Modality, ValidFrom: calendarDate(r.ValidFrom.Time), ValidTo: calendarDate(r.ValidTo.Time), Value: value,
	}, nil
}

// RequestMarketParameterLoad asks to load the value of a market parameter: it is checked now (valid and
// without overlapping another period) and waits as a products.market_parameter_load request until someone
// else approves it (FR-21), once per Idempotency-Key.
func (h *ProductHandlers) RequestMarketParameterLoad(ctx context.Context, req gen.RequestMarketParameterLoadRequestObject) (gen.RequestMarketParameterLoadResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	payload := marketLoadRequest{
		Kind: products.MarketKind(req.Body.Kind), Modality: deref(req.Body.Modality),
		ValidFrom: req.Body.ValidFrom, ValidTo: req.Body.ValidTo, Value: req.Body.Value,
	}
	resp, err := h.commands.idempotent(ctx, p, loadMarketParameter, req.Params.IdempotencyKey, req.Body, func(tx postgres.Tx) (any, error) {
		parameter, err := payload.parameter()
		if err != nil {
			return nil, err
		}
		if err := h.products.CheckMarketParameter(ctx, tx, parameter); err != nil {
			return nil, err
		}
		requested, err := h.approvals.Request(ctx, tx, marketLoadKind, payload, p)
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

// loadMarketParameterOnApproval is the Executor of marketLoadKind: it stores the value. If by then another
// value holds in part of the period, the request is left failed. Its result is the loaded value.
func loadMarketParameterOnApproval(svc *products.Service) approvals.Executor {
	return func(ctx context.Context, tx postgres.Tx, payload json.RawMessage, _ access.Principal) (json.RawMessage, error) {
		var r marketLoadRequest
		if err := json.Unmarshal(payload, &r); err != nil {
			return nil, fmt.Errorf("market parameter load payload: %w", err)
		}
		parameter, err := r.parameter()
		if err != nil {
			return nil, asCommandRejection(err)
		}
		if err := svc.LoadMarketParameter(ctx, tx, parameter); err != nil {
			return nil, asCommandRejection(err)
		}
		return payload, nil
	}
}

// GetMarketParameter reads the value of a market parameter on a day; with no value for that day it is 404
// no_market_parameter.
func (h *ProductHandlers) GetMarketParameter(ctx context.Context, req gen.GetMarketParameterRequestObject) (gen.GetMarketParameterResponseObject, error) {
	modality := deref(req.Params.Modality)
	value, err := h.products.MarketParameter(ctx, h.reader, products.MarketKind(req.Params.Kind), modality, calendarDate(req.Params.On.Time))
	if err != nil {
		return nil, err
	}
	out := gen.MarketParameterValue{Kind: req.Params.Kind, On: req.Params.On, Value: value.String()}
	if modality != "" {
		out.Modality = &modality
	}
	return gen.GetMarketParameter200JSONResponse(out), nil
}

func calendarDate(t time.Time) calendar.Date {
	return calendar.Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
