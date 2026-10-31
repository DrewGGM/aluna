package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"unicode/utf8"

	"github.com/DrewGGM/aluna/internal/access"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// maxIdempotencyKey is the maximum length of Idempotency-Key, in characters, the same that
// infra.idempotency_keys accepts. The generated code does not enforce the maxLength of headers.
const maxIdempotencyKey = 255

var errInvalidIdempotencyKey = fmt.Errorf("Idempotency-Key longer than %d characters or not UTF-8", maxIdempotencyKey)

// operation is the fixed part of a command: its path, the status of its successful response and the IDs the
// path names, which its audit record carries even if the command is rejected.
type operation struct {
	method, path string
	success      int
	targets      []string
}

var (
	postTransaction     = operation{method: http.MethodPost, path: "/v1/transactions", success: http.StatusCreated}
	placeHold           = operation{method: http.MethodPost, path: "/v1/holds", success: http.StatusCreated}
	createLedgerAccount = operation{method: http.MethodPost, path: "/v1/ledger-accounts", success: http.StatusCreated}
)

// resourceOperation is the action action on the resource id of collection ("/v1/holds/{id}/capture"). The
// path carries the id: the fingerprint and the key scope are per resource, so the same key with the same body
// on another resource is not taken for a retry, and the audit record names the resource even if the command
// is rejected.
func resourceOperation(collection string, id ids.ID, action string, success int) operation {
	return operation{
		method: http.MethodPost, path: "/v1/" + collection + "/" + id.String() + "/" + action, success: success,
		targets: []string{id.String()},
	}
}

// commands runs each command in its transaction, with its audit record (FR-40); those that move money also
// run once per Idempotency-Key (architecture §9): the key is claimed and completed in the same transaction
// as the effect. Keys are per entity and subject (keyScope): two entities sharing the database, or two
// subjects of one entity, do not share keys.
type commands struct {
	uow  *postgres.UnitOfWork
	keys *idempotency.Store
}

// storedResponse is a command's response as it is written; that of a command with Idempotency-Key is stored
// and the retry receives it byte for byte.
type storedResponse struct {
	status int
	body   []byte
}

// VisitCreateLedgerAccountResponse writes the response of POST /v1/ledger-accounts.
func (r storedResponse) VisitCreateLedgerAccountResponse(w http.ResponseWriter) error {
	return r.write(w)
}

// VisitPostTransactionResponse writes the stored response of POST /v1/transactions.
func (r storedResponse) VisitPostTransactionResponse(w http.ResponseWriter) error { return r.write(w) }

// VisitPlaceHoldResponse writes the stored response of POST /v1/holds.
func (r storedResponse) VisitPlaceHoldResponse(w http.ResponseWriter) error { return r.write(w) }

// VisitCaptureHoldResponse writes the stored response of POST /v1/holds/{id}/capture.
func (r storedResponse) VisitCaptureHoldResponse(w http.ResponseWriter) error { return r.write(w) }

// VisitReleaseHoldResponse writes the stored response of POST /v1/holds/{id}/release.
func (r storedResponse) VisitReleaseHoldResponse(w http.ResponseWriter) error { return r.write(w) }

// VisitRegisterPartyResponse writes the response of POST /v1/parties.
func (r storedResponse) VisitRegisterPartyResponse(w http.ResponseWriter) error { return r.write(w) }

// VisitRequestReversalResponse writes the stored response of POST /v1/transactions/{id}/reversal.
func (r storedResponse) VisitRequestReversalResponse(w http.ResponseWriter) error { return r.write(w) }

// VisitApproveRequestResponse writes the stored response of POST /v1/approvals/{id}/approval.
func (r storedResponse) VisitApproveRequestResponse(w http.ResponseWriter) error { return r.write(w) }

// VisitRejectRequestResponse writes the response of POST /v1/approvals/{id}/rejection.
func (r storedResponse) VisitRejectRequestResponse(w http.ResponseWriter) error { return r.write(w) }

// write writes the response as it was stored. The Store does not keep the Content-Type: the status gives it,
// because underSavepoint only produces the resource JSON (2xx) or a problem (4xx).
func (r storedResponse) write(w http.ResponseWriter) error {
	contentType := "application/json"
	if r.status >= http.StatusBadRequest {
		contentType = "application/problem+json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(r.status)
	_, err := w.Write(r.body)
	return err
}

// idempotent runs effect for op once per Idempotency-Key key of the principal p. A business 4xx is stored
// too; an error that is not a business one goes up without being stored, so the client can retry. A retry
// with the same key receives the stored response and is audited as a replay.
func (c commands) idempotent(ctx context.Context, p access.Principal, op operation, key string, request any,
	effect func(tx postgres.Tx) (any, error),
) (storedResponse, error) {
	// before validating: a rejection of the key also names the resource
	noteTargets(ctx, op.targets)
	if !utf8.ValidString(key) || utf8.RuneCountInString(key) > maxIdempotencyKey { // an empty key never gets here: the generated code rejects it
		return storedResponse{}, errInvalidIdempotencyKey
	}
	hash, err := requestHash(op, request)
	if err != nil {
		return storedResponse{}, err
	}
	scope := keyScope(p, op)
	return c.audited(ctx, func(tx postgres.Tx) (storedResponse, string, error) {
		claim, err := c.keys.Claim(ctx, tx, scope, key, hash)
		if err != nil {
			return storedResponse{}, "", err
		}
		if !claim.Fresh {
			return storedResponse{status: claim.Status, body: claim.Body}, resultReplay, nil
		}
		resp, result, err := underSavepoint(ctx, tx, op, effect)
		if err != nil {
			return storedResponse{}, "", err
		}
		return resp, result, c.keys.Complete(ctx, tx, scope, key, resp.status, resp.body)
	})
}

// keyScope is the scope of p's keys for op: the entity, the path and the SHA-256 of the sub, which thus fits
// in the scope whatever the issuer.
func keyScope(p access.Principal, op operation) string {
	return fmt.Sprintf("%s %s %s %x", p.LegalEntityID, op.method, op.path, sha256.Sum256([]byte(p.Subject)))
}

// run runs effect for op, a command without Idempotency-Key; a business rejection is rolled back to the
// savepoint and becomes the response.
func (c commands) run(ctx context.Context, op operation, effect func(tx postgres.Tx) (any, error)) (storedResponse, error) {
	noteTargets(ctx, op.targets)
	return c.audited(ctx, func(tx postgres.Tx) (storedResponse, string, error) { return underSavepoint(ctx, tx, op, effect) })
}

// audited runs step in a transaction and stores in it the command's audit record with the response and
// the result that step decided. If the transaction does not commit, the record goes with it and withAudit
// writes it separately.
func (c commands) audited(ctx context.Context, step func(tx postgres.Tx) (storedResponse, string, error)) (storedResponse, error) {
	trail, err := auditTrailFrom(ctx)
	if err != nil {
		return storedResponse{}, err
	}
	var resp storedResponse
	err = c.uow.Do(ctx, func(tx postgres.Tx) error {
		var result string
		var err error
		if resp, result, err = step(tx); err != nil {
			return err
		}
		return trail.record(ctx, tx, resp, result)
	})
	trail.committed = err == nil
	return resp, err
}

// underSavepoint runs effect under a savepoint and turns it into the command's response and its result: ok
// or the problem code. A business error is rolled back to there, also if the database aborted the
// transaction, and its problem is the response. Any other error, including the conflicts that UnitOfWork
// retries, goes up as is. The body ends in '\n', like that of the json.Encoder used by the generated code
// and WriteProblem.
func underSavepoint(ctx context.Context, tx postgres.Tx, op operation, effect func(tx postgres.Tx) (any, error)) (storedResponse, string, error) {
	if _, err := tx.Exec(ctx, "SAVEPOINT command_effect"); err != nil {
		return storedResponse{}, "", fmt.Errorf("command savepoint: %w", err)
	}
	result, effectErr := effect(tx)
	if effectErr == nil {
		body, err := json.Marshal(result)
		if err != nil {
			return storedResponse{}, "", fmt.Errorf("command response: %w", err)
		}
		return storedResponse{status: op.success, body: append(body, '\n')}, resultOK, nil
	}
	p, isBusiness := problemFor(effectErr)
	if !isBusiness {
		return storedResponse{}, "", effectErr
	}
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT command_effect"); err != nil {
		return storedResponse{}, "", fmt.Errorf("roll back command: %w", err)
	}
	body, err := json.Marshal(p.complete(ctx, op.path))
	if err != nil {
		return storedResponse{}, "", fmt.Errorf("command problem: %w", err)
	}
	return storedResponse{status: p.Status, body: append(body, '\n')}, p.Code, nil
}

// requestHash is the request fingerprint: SHA-256 of the method, the path and the canonical body (the body
// already read and serialized again, so whitespace and key order do not change the fingerprint).
func requestHash(op operation, request any) ([32]byte, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return [32]byte{}, fmt.Errorf("request fingerprint: %w", err)
	}
	return sha256.Sum256(append([]byte(op.method+" "+op.path+"\n"), body...)), nil
}
