package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/webhooks"
)

// WebhookHandlers serve the operations of the webhooks module (FR-46): the Events API and the webhook endpoints.
type WebhookHandlers struct {
	webhooks *webhooks.Service
	commands commands
	reader   postgres.Querier
}

// NewWebhookHandlers creates the webhooks handlers; reader is what the events are read from.
func NewWebhookHandlers(w *webhooks.Service, uow *postgres.UnitOfWork, reader postgres.Querier) *WebhookHandlers {
	return &WebhookHandlers{webhooks: w, commands: commands{uow: uow}, reader: reader}
}

var registerWebhookEndpoint = operation{method: http.MethodPost, path: "/v1/webhook-endpoints", success: http.StatusCreated}

// RegisterWebhookEndpoint registers an endpoint of the token's entity for its events, and answers with its secret,
// which no other response shows.
func (h *WebhookHandlers) RegisterWebhookEndpoint(ctx context.Context, req gen.RegisterWebhookEndpointRequestObject) (gen.RegisterWebhookEndpointResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := h.commands.run(ctx, registerWebhookEndpoint, func(tx postgres.Tx) (any, error) {
		e, err := h.webhooks.RegisterEndpoint(ctx, tx, p.LegalEntityID, req.Body.Url, p.Subject)
		if err != nil {
			return nil, err
		}
		return gen.WebhookEndpoint{Id: [16]byte(e.ID), Url: e.URL, Secret: e.Secret, CreatedAt: e.CreatedAt}, nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// RevokeWebhookEndpoint revokes an endpoint of the token's entity: it gets no more events, and its pending
// deliveries fail.
func (h *WebhookHandlers) RevokeWebhookEndpoint(ctx context.Context, req gen.RevokeWebhookEndpointRequestObject) (gen.RevokeWebhookEndpointResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	id := ids.ID(req.Id)
	op := resourceOperation("webhook-endpoints", id, "revocation", http.StatusOK)
	resp, err := h.commands.run(ctx, op, func(tx postgres.Tx) (any, error) {
		e, err := h.webhooks.RevokeEndpoint(ctx, tx, p.LegalEntityID, id, p.Subject)
		if err != nil {
			return nil, err
		}
		return gen.RevokedWebhookEndpoint{Id: req.Id, Url: e.URL, RevokedAt: e.RevokedAt}, nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// ListEvents reads a page of the events of the token's entity of the last 30 days, as CloudEvents.
func (h *WebhookHandlers) ListEvents(ctx context.Context, req gen.ListEventsRequestObject) (gen.ListEventsResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	cursor, limit := pageOf(req.Params.Cursor, req.Params.Limit)
	typ := deref(req.Params.Type)
	page, err := h.webhooks.Events(ctx, h.reader, p.LegalEntityID, typ, cursor, limit)
	if err != nil {
		return nil, err
	}
	events := make([]gen.CloudEvent, len(page.Events))
	for i, e := range page.Events {
		if events[i], err = json.Marshal(e); err != nil {
			return nil, fmt.Errorf("event %s: %w", e.ID, err)
		}
	}
	return gen.ListEvents200JSONResponse{Events: events, NextCursor: page.Next, HasMore: page.HasMore}, nil
}
