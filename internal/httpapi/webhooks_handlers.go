package httpapi

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/webhooks"
)

// WebhookHandlers serve the operations of the webhooks module: the Events API (FR-46).
type WebhookHandlers struct {
	webhooks *webhooks.Service
	reader   postgres.Querier
}

// NewWebhookHandlers creates the webhooks handlers; reader is what the events are read from.
func NewWebhookHandlers(w *webhooks.Service, reader postgres.Querier) *WebhookHandlers {
	return &WebhookHandlers{webhooks: w, reader: reader}
}

// ListEvents reads a page of the events of the token's entity of the last 30 days, as CloudEvents.
func (h *WebhookHandlers) ListEvents(ctx context.Context, req gen.ListEventsRequestObject) (gen.ListEventsResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	cursor, typ, limit := deref(req.Params.Cursor), deref(req.Params.Type), defaultPageSize
	if req.Params.Limit != nil {
		limit = *req.Params.Limit
	}
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
