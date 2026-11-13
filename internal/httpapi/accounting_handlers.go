package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/DrewGGM/aluna/internal/access"
	"github.com/DrewGGM/aluna/internal/accounting"
	"github.com/DrewGGM/aluna/internal/approvals"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// Maker-checker request kind of a load of CUIF mappings and the permission to request and to approve it.
const (
	cuifLoadKind       = "accounting.cuif_mappings_load"
	cuifLoadPermission = "accounting.cuif_mappings.load"
)

var loadCuifMappings = operation{method: http.MethodPost, path: "/v1/cuif-mappings", success: http.StatusAccepted}

// AccountingHandlers implements the contract's accounting operations (api/openapi.yaml).
type AccountingHandlers struct {
	accounting *accounting.Service
	approvals  *approvals.Service
	commands   commands
	reader     postgres.Querier
}

// NewAccountingHandlers creates the accounting handlers and registers in a the load of CUIF mappings, which goes
// through maker-checker. keys stores the responses of the commands with Idempotency-Key, and reader is what the GL
// is read from.
func NewAccountingHandlers(s *accounting.Service, a *approvals.Service, uow *postgres.UnitOfWork, keys *idempotency.Store, reader postgres.Querier) *AccountingHandlers {
	a.RegisterKind(cuifLoadKind, cuifLoadPermission, loadCuifMappingsOnApproval(s))
	return &AccountingHandlers{accounting: s, approvals: a, commands: commands{uow: uow, keys: keys}, reader: reader}
}

// GetDailyGL reads the GL of a day of the token's entity.
func (h *AccountingHandlers) GetDailyGL(ctx context.Context, req gen.GetDailyGLRequestObject) (gen.GetDailyGLResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	gl, err := h.accounting.GL(ctx, h.reader, p.LegalEntityID, calendarDate(req.Params.Date.Time))
	if err != nil {
		return nil, err
	}
	out := gen.DailyGL{Date: req.Params.Date, ProjectedAt: gl.ProjectedAt, Lines: make([]gen.GLLine, len(gl.Lines))}
	for i, l := range gl.Lines {
		out.Lines[i] = gen.GLLine{CuifCode: l.CUIFCode, Debits: moneyJSON(l.Debits), Credits: moneyJSON(l.Credits)}
	}
	return gen.GetDailyGL200JSONResponse(out), nil
}

// cuifLoadRequest is the payload of a load request: the file the checker approves.
type cuifLoadRequest struct {
	CSV string `json:"csv"`
}

// RequestCuifMappingsLoad asks to load a file of CUIF mappings into the token's entity: it is checked now and waits
// as an accounting.cuif_mappings_load request until someone else approves it (FR-36), once per Idempotency-Key.
func (h *AccountingHandlers) RequestCuifMappingsLoad(ctx context.Context, req gen.RequestCuifMappingsLoadRequestObject) (gen.RequestCuifMappingsLoadResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := h.commands.idempotent(ctx, p, loadCuifMappings, req.Params.IdempotencyKey, req.Body, func(tx postgres.Tx) (any, error) {
		mappings, err := accounting.ParseMappings(strings.NewReader(req.Body.Csv))
		if err != nil {
			return nil, err
		}
		if err := h.accounting.CheckMappings(ctx, tx, p.LegalEntityID, mappings); err != nil {
			return nil, err
		}
		requested, err := h.approvals.Request(ctx, tx, cuifLoadKind, cuifLoadRequest{CSV: req.Body.Csv}, p)
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

// loadCuifMappingsOnApproval is the Executor of cuifLoadKind: it stores the file in the approver's entity. One that
// overlaps what was stored meanwhile leaves the request failed.
func loadCuifMappingsOnApproval(s *accounting.Service) approvals.Executor {
	return func(ctx context.Context, tx postgres.Tx, payload json.RawMessage, checker access.Principal) (json.RawMessage, error) {
		var r cuifLoadRequest
		if err := json.Unmarshal(payload, &r); err != nil {
			return nil, fmt.Errorf("CUIF mappings load payload: %w", err)
		}
		mappings, err := accounting.ParseMappings(strings.NewReader(r.CSV))
		if err != nil {
			return nil, asCommandRejection(err)
		}
		if err := s.LoadMappings(ctx, tx, checker.LegalEntityID, mappings); err != nil {
			return nil, asCommandRejection(err)
		}
		result, err := json.Marshal(cuifLoadResult{Mappings: len(mappings)})
		if err != nil {
			return nil, fmt.Errorf("CUIF mappings load result: %w", err)
		}
		return result, nil
	}
}

// cuifLoadResult is what an approved load returns: how many mappings it stored. The file stays in the payload.
type cuifLoadResult struct {
	Mappings int `json:"mappings"`
}
