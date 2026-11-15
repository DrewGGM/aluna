package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/DrewGGM/aluna/internal/access"
	"github.com/DrewGGM/aluna/internal/approvals"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
)

var errInvalidApprovalStatus = errors.New("unknown request status")

// ApprovalHandlers implements the contract's maker-checker operations (api/openapi.yaml).
type ApprovalHandlers struct {
	approvals *approvals.Service
	commands  commands
	reader    postgres.Querier
}

// NewApprovalHandlers creates the handlers for the requests of svc. keys stores the responses of the
// approvals (with Idempotency-Key: approving runs the command); reader serves the reads.
func NewApprovalHandlers(svc *approvals.Service, uow *postgres.UnitOfWork, keys *idempotency.Store, reader postgres.Querier) *ApprovalHandlers {
	return &ApprovalHandlers{approvals: svc, commands: commands{uow: uow, keys: keys}, reader: reader}
}

// ListApprovals lists the requests of the token's entity.
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

// ApproveRequest approves a request of the token's entity and runs its command, once per Idempotency-Key.
func (h *ApprovalHandlers) ApproveRequest(ctx context.Context, req gen.ApproveRequestRequestObject) (gen.ApproveRequestResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	id := ids.ID(req.Id)
	resp, err := h.commands.idempotent(ctx, p, resourceOperation("approvals", id, "approval", http.StatusOK), req.Params.IdempotencyKey, nil, func(tx postgres.Tx) (any, error) {
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

// RejectRequest rejects a request of the token's entity.
func (h *ApprovalHandlers) RejectRequest(ctx context.Context, req gen.RejectRequestRequestObject) (gen.RejectRequestResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	id := ids.ID(req.Id)
	resp, err := h.commands.run(ctx, resourceOperation("approvals", id, "rejection", http.StatusOK), func(tx postgres.Tx) (any, error) {
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

// requestApproval asks, within tx, for a maker-checker request of kind with payload, made by p, and returns it as the
// API shows it.
func requestApproval(ctx context.Context, a *approvals.Service, tx postgres.Tx, kind string, payload any, p access.Principal) (gen.Approval, error) {
	requested, err := a.Request(ctx, tx, kind, payload, p)
	if err != nil {
		return gen.Approval{}, err
	}
	return toApprovalJSON(requested), nil
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
	if r.Failure != "" {
		a.Failure = &r.Failure
	}
	return a
}

// onApproval is the Executor of a kind whose payload is an In and whose result is an Out: it runs command as
// the approver, in the approval's transaction. Every error of command goes through asCommandRejection, so a
// business rule leaves the request failed and anything else leaves it pending. A payload that does not decode
// or a result that does not encode is not a business rule: it goes up as is, labeled with name.
func onApproval[In, Out any](name string, command func(ctx context.Context, tx postgres.Tx, in In, checker access.Principal) (Out, error)) approvals.Executor {
	return func(ctx context.Context, tx postgres.Tx, payload json.RawMessage, checker access.Principal) (json.RawMessage, error) {
		var in In
		if err := json.Unmarshal(payload, &in); err != nil {
			return nil, fmt.Errorf("%s payload: %w", name, err)
		}
		out, err := command(ctx, tx, in, checker)
		if err != nil {
			return nil, asCommandRejection(err)
		}
		result, err := json.Marshal(out)
		if err != nil {
			return nil, fmt.Errorf("%s result: %w", name, err)
		}
		return result, nil
	}
}

// asCommandRejection makes an error of an approved command that is a business rule (a 4xx in the problems
// table) its rejection with its problem code: the request is left failed, saying why. Any other error goes up as
// is, so the request stays pending and can be approved later.
func asCommandRejection(err error) error {
	if p, known := problemFor(err); known && p.Status < http.StatusInternalServerError {
		return approvals.Rejected(p.Code, err)
	}
	return err
}
