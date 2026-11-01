package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/DrewGGM/aluna/internal/access"
	"github.com/DrewGGM/aluna/internal/approvals"
	"github.com/DrewGGM/aluna/internal/compliance"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// Maker-checker request kind of a screening case's resolution (FR-42, FR-39) and the permission to request
// and to approve it.
const (
	screeningResolutionKind       = "compliance.screening_resolution"
	screeningResolutionPermission = "compliance.screening.resolve"
)

// errInvalidResolution is a resolution other than cleared or confirmed: the generated code does not reject
// it.
var errInvalidResolution = errors.New("resolution is cleared or confirmed")

// ComplianceHandlers implements the contract's screening operations (api/openapi.yaml).
type ComplianceHandlers struct {
	compliance *compliance.Service
	approvals  *approvals.Service
	commands   commands
}

// NewComplianceHandlers creates the compliance handlers and registers in a the resolution request kind,
// which goes through maker-checker. keys stores the responses of the commands with Idempotency-Key.
func NewComplianceHandlers(c *compliance.Service, a *approvals.Service, uow *postgres.UnitOfWork, keys *idempotency.Store) *ComplianceHandlers {
	a.RegisterKind(screeningResolutionKind, screeningResolutionPermission, resolveOnApproval(c))
	return &ComplianceHandlers{compliance: c, approvals: a, commands: commands{uow: uow, keys: keys}}
}

// resolutionRequest is the payload of a resolution request, kept as evidence: which case, and what it
// becomes (cleared or confirmed).
type resolutionRequest struct {
	CaseID     ids.ID                `json:"case_id"`
	Resolution compliance.CaseStatus `json:"resolution"`
}

// resolutionResult is what an approved resolution returns.
type resolutionResult struct {
	CaseID ids.ID                `json:"case_id"`
	Status compliance.CaseStatus `json:"status"`
}

// ResolveScreeningCase asks for the resolution of a screening case of the token's entity: it waits as a
// compliance.screening_resolution request until someone else approves it (FR-39), once per Idempotency-Key.
// The case is checked to be pending now and again when approving.
func (h *ComplianceHandlers) ResolveScreeningCase(ctx context.Context, req gen.ResolveScreeningCaseRequestObject) (gen.ResolveScreeningCaseResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	id := ids.ID(req.Id)
	op := resourceOperation("screening-cases", id, "resolution", http.StatusAccepted)
	noteTargets(ctx, op.targets) // before validating the body: its rejection also names the case
	resolution := req.Body.Resolution
	if !resolution.Valid() {
		return nil, fmt.Errorf("%w, not %q", errInvalidResolution, resolution)
	}
	payload := resolutionRequest{CaseID: id, Resolution: compliance.CaseStatus(resolution)}
	resp, err := h.commands.idempotent(ctx, p, op, req.Params.IdempotencyKey, req.Body, func(tx postgres.Tx) (any, error) {
		if err := h.compliance.CheckResolution(ctx, tx, p.LegalEntityID, id); err != nil {
			return nil, err
		}
		requested, err := h.approvals.Request(ctx, tx, screeningResolutionKind, payload, p)
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

// resolveOnApproval is the Executor of screeningResolutionKind: it resolves the case in the approver's
// entity, with the approver as who resolved it. A case no longer pending, or a subject that cannot take the
// resolution, leaves the request failed.
func resolveOnApproval(c *compliance.Service) approvals.Executor {
	return func(ctx context.Context, tx postgres.Tx, payload json.RawMessage, checker access.Principal) (json.RawMessage, error) {
		var r resolutionRequest
		if err := json.Unmarshal(payload, &r); err != nil {
			return nil, fmt.Errorf("screening resolution payload: %w", err)
		}
		resolved, err := c.Resolve(ctx, tx, checker.LegalEntityID, r.CaseID, r.Resolution, checker.Subject)
		if err != nil {
			return nil, asCommandRejection(err)
		}
		result, err := json.Marshal(resolutionResult{CaseID: resolved.ID, Status: resolved.Status})
		if err != nil {
			return nil, fmt.Errorf("screening resolution result: %w", err)
		}
		return result, nil
	}
}
