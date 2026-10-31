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
	"github.com/DrewGGM/aluna/internal/party"
	"github.com/DrewGGM/aluna/internal/postgres"
)

var registerParty = operation{method: http.MethodPost, path: "/v1/parties", success: http.StatusCreated}

// Maker-checker request kind of a KYC status change (FR-16, FR-39) and the permission to request and to
// approve it.
const (
	kycChangeKind       = "party.kyc_status_change"
	kycChangePermission = "party.kyc.change"
)

// PartyHandlers implements the contract's party operations (api/openapi.yaml).
type PartyHandlers struct {
	parties   *party.Service
	approvals *approvals.Service
	commands  commands
	reader    postgres.Querier
}

// NewPartyHandlers creates the party handlers and registers in a the KYC status change request kind, which
// goes through maker-checker. keys stores the responses of the commands with Idempotency-Key; reader serves
// the reads outside a transaction.
func NewPartyHandlers(p *party.Service, a *approvals.Service, uow *postgres.UnitOfWork, keys *idempotency.Store, reader postgres.Querier) *PartyHandlers {
	a.RegisterKind(kycChangeKind, kycChangePermission, changeKYCOnApproval(p))
	return &PartyHandlers{parties: p, approvals: a, commands: commands{uow: uow, keys: keys}, reader: reader}
}

// RegisterParty registers a party of the token's entity, in KYC pending.
func (h *PartyHandlers) RegisterParty(ctx context.Context, req gen.RegisterPartyRequestObject) (gen.RegisterPartyResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if req.Body.Pep == nil { // a missing PEP flag is not "not PEP": fail closed
		return nil, fmt.Errorf("%w: missing pep", party.ErrInvalidParty)
	}
	in := party.RegisterInput{
		LegalEntityID: p.LegalEntityID, Kind: party.Kind(req.Body.Kind), ExternalRef: req.Body.ExternalRef,
		RiskLevel: party.RiskLevel(req.Body.RiskLevel), PEP: *req.Body.Pep,
	}
	if req.Body.Consents != nil {
		in.Consents = *req.Body.Consents
	}
	resp, err := h.commands.run(ctx, registerParty, func(tx postgres.Tx) (any, error) {
		registered, err := h.parties.Register(ctx, tx, in)
		if err != nil {
			return nil, err
		}
		return toPartyJSON(registered), nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// GetParty reads a party of the token's entity; another entity's party does not exist for it.
func (h *PartyHandlers) GetParty(ctx context.Context, req gen.GetPartyRequestObject) (gen.GetPartyResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	got, err := h.parties.Get(ctx, h.reader, p.LegalEntityID, ids.ID(req.Id))
	if err != nil {
		return nil, err
	}
	return gen.GetParty200JSONResponse(toPartyJSON(got)), nil
}

// kycChangeRequest is the payload of a KYC change request: what the checker approves, against which version
// of the party, and what is recorded. It carries no free text: requests are kept as evidence and cannot be
// erased.
type kycChangeRequest struct {
	PartyID   ids.ID          `json:"party_id"`
	Version   int             `json:"version"`
	KYCStatus party.KYCStatus `json:"kyc_status"`
}

// kycChangeResult is what an approved KYC change returns: only the new state, because the request is read
// with approvals.read, which does not grant reading parties.
type kycChangeResult struct {
	PartyID   ids.ID          `json:"party_id"`
	Version   int             `json:"version"`
	KYCStatus party.KYCStatus `json:"kyc_status"`
}

// RequestKYCStatusChange asks for a KYC status change of a party of the token's entity: it waits as a
// party.kyc_status_change request until someone else approves it (FR-39), once per Idempotency-Key. The
// transition is checked now and again when approving.
func (h *PartyHandlers) RequestKYCStatusChange(ctx context.Context, req gen.RequestKYCStatusChangeRequestObject) (gen.RequestKYCStatusChangeResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	id := ids.ID(req.Id)
	payload := kycChangeRequest{PartyID: id, KYCStatus: party.KYCStatus(req.Body.KycStatus)}
	op := resourceOperation("parties", id, "kyc-status-changes", http.StatusAccepted)
	resp, err := h.commands.idempotent(ctx, p, op, req.Params.IdempotencyKey, req.Body, func(tx postgres.Tx) (any, error) {
		var err error
		if payload.Version, err = h.parties.CheckKYCChange(ctx, tx, p.LegalEntityID, id, payload.KYCStatus); err != nil {
			return nil, err
		}
		requested, err := h.approvals.Request(ctx, tx, kycChangeKind, payload, p)
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

// changeKYCOnApproval is the Executor of kycChangeKind: it records the new version of the party in the
// approver's entity. If the party changed since the request (another version, even with the same status),
// the request is left failed.
func changeKYCOnApproval(parties *party.Service) approvals.Executor {
	return func(ctx context.Context, tx postgres.Tx, payload json.RawMessage, checker access.Principal) (json.RawMessage, error) {
		var r kycChangeRequest
		if err := json.Unmarshal(payload, &r); err != nil {
			return nil, fmt.Errorf("KYC change payload: %w", err)
		}
		changed, err := parties.ChangeKYC(ctx, tx, checker.LegalEntityID, r.PartyID, r.Version, r.KYCStatus)
		if err != nil {
			return nil, asCommandRejection(err)
		}
		result, err := json.Marshal(kycChangeResult{PartyID: changed.ID, Version: changed.Current.Number, KYCStatus: changed.Current.KYCStatus})
		if err != nil {
			return nil, fmt.Errorf("KYC change result: %w", err)
		}
		return result, nil
	}
}

func toPartyJSON(p party.Party) gen.Party {
	v := p.Current
	return gen.Party{
		Id: [16]byte(p.ID), Kind: gen.PartyKind(p.Kind), ExternalRef: p.ExternalRef, CreatedAt: p.CreatedAt,
		Version: v.Number, KycStatus: gen.KYCStatus(v.KYCStatus), RiskLevel: gen.RiskLevel(v.RiskLevel), Pep: v.PEP,
		Consents: v.Consents, ValidFrom: v.ValidFrom,
	}
}
