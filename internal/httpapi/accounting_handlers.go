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
	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/reconciliation"
)

// Maker-checker request kinds of a load of CUIF mappings and of a period's close, and the permissions to request and
// to approve them.
const (
	cuifLoadKind          = "accounting.cuif_mappings_load"
	cuifLoadPermission    = "accounting.cuif_mappings.load"
	periodCloseKind       = "accounting.period_close"
	periodClosePermission = "accounting.periods.close"
)

var loadCuifMappings = operation{method: http.MethodPost, path: "/v1/cuif-mappings", success: http.StatusAccepted}

// AccountingHandlers implements the contract's accounting operations (api/openapi.yaml).
type AccountingHandlers struct {
	accounting *accounting.Service
	approvals  *approvals.Service
	commands   commands
	reader     postgres.Querier
}

// NewAccountingHandlers creates the accounting handlers and registers in a the load of CUIF mappings and the close of
// a period, which go through maker-checker; the close asks recon for the breaks open. keys stores the responses of the
// commands with Idempotency-Key, and reader is what the GL is read from.
func NewAccountingHandlers(s *accounting.Service, recon *reconciliation.Service, a *approvals.Service, uow *postgres.UnitOfWork, keys *idempotency.Store,
	reader postgres.Querier,
) *AccountingHandlers {
	a.RegisterKind(cuifLoadKind, cuifLoadPermission, loadCuifMappingsOnApproval(s))
	a.RegisterKind(periodCloseKind, periodClosePermission, closePeriodOnApproval(s, recon))
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

// periodCloseRequest is the payload of a close request.
type periodCloseRequest struct {
	Period string `json:"period"`
}

// periodCloseResult is what an approved close returns.
type periodCloseResult struct {
	Period string `json:"period"`
	Status string `json:"status"`
}

// RequestPeriodClose asks to close a period of the token's entity (FR-37, US-14): it is checked now and waits as an
// accounting.period_close request until someone else approves it, once per Idempotency-Key.
func (h *AccountingHandlers) RequestPeriodClose(ctx context.Context, req gen.RequestPeriodCloseRequestObject) (gen.RequestPeriodCloseResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	// the period goes into the idempotency key's scope and into the audit before it is checked
	if err := calendar.ValidatePeriod(req.Period); err != nil {
		return nil, err
	}
	op := operation{method: http.MethodPost, path: "/v1/accounting-periods/" + req.Period + "/closure", success: http.StatusAccepted, targets: []string{req.Period}}
	resp, err := h.commands.idempotent(ctx, p, op, req.Params.IdempotencyKey, periodCloseRequest{Period: req.Period}, func(tx postgres.Tx) (any, error) {
		if err := h.accounting.CheckPeriodClose(ctx, tx, p.LegalEntityID, req.Period); err != nil {
			return nil, err
		}
		requested, err := h.approvals.Request(ctx, tx, periodCloseKind, periodCloseRequest{Period: req.Period}, p)
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

// closePeriodOnApproval is the Executor of periodCloseKind: it closes the period in the approver's entity, by the
// approver, if recon has no break of the entity open. A period that does not square, or open breaks, leave the request
// failed with why, and the period open.
func closePeriodOnApproval(s *accounting.Service, recon *reconciliation.Service) approvals.Executor {
	return func(ctx context.Context, tx postgres.Tx, payload json.RawMessage, checker access.Principal) (json.RawMessage, error) {
		var r periodCloseRequest
		if err := json.Unmarshal(payload, &r); err != nil {
			return nil, fmt.Errorf("period close payload: %w", err)
		}
		anyOpenBreak := func(ctx context.Context) (bool, error) {
			open, err := recon.Breaks(ctx, tx, checker.LegalEntityID, reconciliation.BreakOpen, "", 1)
			return len(open.Breaks) > 0, err
		}
		if err := s.ClosePeriod(ctx, tx, checker.LegalEntityID, r.Period, checker.Subject, anyOpenBreak); err != nil {
			return nil, asCommandRejection(err)
		}
		result, err := json.Marshal(periodCloseResult{Period: r.Period, Status: "closed"})
		if err != nil {
			return nil, fmt.Errorf("period close result: %w", err)
		}
		return result, nil
	}
}
