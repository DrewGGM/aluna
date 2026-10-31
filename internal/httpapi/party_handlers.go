package httpapi

import (
	"context"
	"fmt"
	"net/http"

	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/party"
	"github.com/DrewGGM/aluna/internal/postgres"
)

var registerParty = operation{method: http.MethodPost, path: "/v1/parties", success: http.StatusCreated}

// PartyHandlers implements the contract's party operations (api/openapi.yaml).
type PartyHandlers struct {
	parties  *party.Service
	commands commands
	reader   postgres.Querier
}

// NewPartyHandlers creates the party handlers; reader serves the reads outside a transaction.
func NewPartyHandlers(p *party.Service, uow *postgres.UnitOfWork, reader postgres.Querier) *PartyHandlers {
	return &PartyHandlers{parties: p, commands: commands{uow: uow}, reader: reader}
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

func toPartyJSON(p party.Party) gen.Party {
	v := p.Current
	return gen.Party{
		Id: [16]byte(p.ID), Kind: gen.PartyKind(p.Kind), ExternalRef: p.ExternalRef, CreatedAt: p.CreatedAt,
		Version: v.Number, KycStatus: gen.KYCStatus(v.KYCStatus), RiskLevel: gen.RiskLevel(v.RiskLevel), Pep: v.PEP,
		Consents: v.Consents, ValidFrom: v.ValidFrom,
	}
}
