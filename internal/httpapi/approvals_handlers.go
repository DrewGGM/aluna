package httpapi

import (
	"context"
	"errors"

	"github.com/DrewGGM/aluna/internal/approvals"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
)

var errInvalidApprovalStatus = errors.New("estado de solicitud desconocido")

// ApprovalHandlers implementa las operaciones de maker-checker del contrato (api/openapi.yaml).
type ApprovalHandlers struct {
	approvals *approvals.Service
	commands  commands
	reader    postgres.Querier
}

// NewApprovalHandlers crea los handlers de las solicitudes de svc. keys guarda las respuestas de las
// aprobaciones (con Idempotency-Key: aprobar corre el comando); reader sirve las lecturas.
func NewApprovalHandlers(svc *approvals.Service, uow *postgres.UnitOfWork, keys *idempotency.Store, reader postgres.Querier) *ApprovalHandlers {
	return &ApprovalHandlers{approvals: svc, commands: commands{uow: uow, keys: keys}, reader: reader}
}

// ListApprovals lista las solicitudes de la entidad del token.
func (h *ApprovalHandlers) ListApprovals(ctx context.Context, req gen.ListApprovalsRequestObject) (gen.ListApprovalsResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	var status approvals.Status
	if req.Params.Status != nil {
		if !req.Params.Status.Valid() {
			return nil, errInvalidApprovalStatus
		}
		status = approvals.Status(*req.Params.Status)
	}
	list, err := h.approvals.List(ctx, h.reader, p.LegalEntityID, status)
	if err != nil {
		return nil, err
	}
	items := make([]gen.Approval, len(list))
	for i, r := range list {
		items[i] = toApprovalJSON(r)
	}
	return gen.ListApprovals200JSONResponse{Items: items}, nil
}

// ApproveRequest aprueba una solicitud de la entidad del token y corre su comando, una sola vez por
// Idempotency-Key.
func (h *ApprovalHandlers) ApproveRequest(ctx context.Context, req gen.ApproveRequestRequestObject) (gen.ApproveRequestResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	id := ids.ID(req.Id)
	resp, err := h.commands.idempotent(ctx, p, approvalOperation(id, "approval"), req.Params.IdempotencyKey, nil, func(tx postgres.Tx) (any, error) {
		approved, err := h.approvals.Approve(ctx, tx, id, p)
		if err != nil {
			return nil, err
		}
		return toApprovalJSON(approved), nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// RejectRequest rechaza una solicitud de la entidad del token.
func (h *ApprovalHandlers) RejectRequest(ctx context.Context, req gen.RejectRequestRequestObject) (gen.RejectRequestResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	id := ids.ID(req.Id)
	resp, err := h.commands.run(ctx, approvalOperation(id, "rejection"), func(tx postgres.Tx) (any, error) {
		rejected, err := h.approvals.Reject(ctx, tx, id, p, req.Body.Reason)
		if err != nil {
			return nil, err
		}
		return toApprovalJSON(rejected), nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func toApprovalJSON(r approvals.Request) gen.Approval {
	a := gen.Approval{
		Id: [16]byte(r.ID), Kind: r.Kind, Status: gen.ApprovalStatus(r.Status), Payload: r.Payload, Maker: r.Maker,
		CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt, DecidedAt: r.DecidedAt,
	}
	if r.Result != nil {
		a.Result = &r.Result
	}
	if r.Checker != "" {
		a.Checker = &r.Checker
	}
	if r.Reason != "" {
		a.Reason = &r.Reason
	}
	return a
}
