package httpapi

import (
	"context"
	"fmt"
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/reconciliation"
)

var importStatement = operation{method: http.MethodPost, path: "/v1/reconciliation/statements", success: http.StatusCreated}

// ReconciliationHandlers implements the contract's reconciliation operations (api/openapi.yaml).
type ReconciliationHandlers struct {
	reconciliation *reconciliation.Service
	commands       commands
	reader         postgres.Querier
}

// NewReconciliationHandlers creates the reconciliation handlers; keys stores the responses of the commands with
// Idempotency-Key, and reader is what the breaks are read from.
func NewReconciliationHandlers(r *reconciliation.Service, uow *postgres.UnitOfWork, keys *idempotency.Store, reader postgres.Querier) *ReconciliationHandlers {
	return &ReconciliationHandlers{reconciliation: r, commands: commands{uow: uow, keys: keys}, reader: reader}
}

// ImportStatement imports a rail's statement into the token's entity, once per Idempotency-Key and once by its
// reference.
func (h *ReconciliationHandlers) ImportStatement(ctx context.Context, req gen.ImportStatementRequestObject) (gen.ImportStatementResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	lines, err := statementLines(req.Body.Lines)
	if err != nil {
		return nil, err
	}
	in := reconciliation.Statement{
		LegalEntityID: p.LegalEntityID, Rail: string(req.Body.Rail), Ref: req.Body.StatementRef, Date: req.Body.StatementDate.Time,
		ImportedBy: p.Subject, Lines: lines,
	}
	resp, err := h.commands.idempotent(ctx, p, importStatement, req.Params.IdempotencyKey, req.Body, func(tx postgres.Tx) (any, error) {
		result, err := h.reconciliation.ImportStatement(ctx, tx, in)
		if err != nil {
			return nil, err
		}
		return gen.StatementImport{Id: [16]byte(result.ID), Lines: result.Lines, Matched: result.Matched, Exceptions: result.Exceptions}, nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// statementLines are the lines of a statement's body; none if the body has none, which is not an empty list.
func statementLines(body []gen.StatementLine) ([]reconciliation.StatementLine, error) {
	var lines []reconciliation.StatementLine
	if body != nil {
		lines = make([]reconciliation.StatementLine, 0, len(body))
	}
	for _, l := range body {
		amount, err := amountOf(l.Amount)
		if err != nil {
			return nil, fmt.Errorf("statement line: %w", err)
		}
		lines = append(lines, reconciliation.StatementLine{
			EndToEndID: l.EndToEndId, Direction: reconciliation.Direction(l.Direction), Amount: amount, BookingDate: l.BookingDate.Time,
		})
	}
	return lines, nil
}

// ListBreaks lists the breaks of the token's entity, newest first.
func (h *ReconciliationHandlers) ListBreaks(ctx context.Context, req gen.ListBreaksRequestObject) (gen.ListBreaksResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	cursor, limit := "", defaultPageSize
	if req.Params.Cursor != nil {
		cursor = *req.Params.Cursor
	}
	if req.Params.Limit != nil {
		limit = *req.Params.Limit
	}
	var status reconciliation.BreakStatus
	if req.Params.Status != nil {
		status = reconciliation.BreakStatus(*req.Params.Status)
	}
	page, err := h.reconciliation.Breaks(ctx, h.reader, p.LegalEntityID, status, cursor, limit)
	if err != nil {
		return nil, err
	}
	list := gen.BreakList{Breaks: make([]gen.Break, len(page.Breaks))}
	for i, b := range page.Breaks {
		list.Breaks[i] = toBreakJSON(b)
	}
	if page.Next != "" {
		list.NextCursor = &page.Next
	}
	return gen.ListBreaks200JSONResponse(list), nil
}

// ResolveBreak resolves a break of the token's entity with its cause, once per Idempotency-Key.
func (h *ReconciliationHandlers) ResolveBreak(ctx context.Context, req gen.ResolveBreakRequestObject) (gen.ResolveBreakResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	id := ids.ID(req.Id)
	op := resourceOperation("reconciliation/breaks", id, "resolution", http.StatusOK)
	resp, err := h.commands.idempotent(ctx, p, op, req.Params.IdempotencyKey, req.Body, func(tx postgres.Tx) (any, error) {
		resolved, err := h.reconciliation.ResolveBreak(ctx, tx, p.LegalEntityID, id, req.Body.Cause, p.Subject)
		if err != nil {
			return nil, err
		}
		return toBreakJSON(resolved), nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func toBreakJSON(b reconciliation.Break) gen.Break {
	out := gen.Break{
		Id: [16]byte(b.ID), Control: string(b.Control), Subject: b.Subject, Difference: moneyJSON(b.Difference), Status: gen.BreakStatus(b.Status),
		Owner: b.Owner, OpenedAt: b.OpenedAt,
	}
	if b.StatementID != (ids.ID{}) {
		id := openapi_types.UUID(b.StatementID)
		out.StatementId = &id
	}
	if b.Reason != "" {
		reason := string(b.Reason)
		out.Reason = &reason
	}
	if r := b.Resolution; r != nil {
		out.Cause, out.ResolvedBy, out.ResolvedAt = &r.Cause, &r.By, &r.At
	}
	return out
}
