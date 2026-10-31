// Package approvals is the cross-cutting maker-checker (FR-39, ADR-0014): a sensitive command is kept as a
// pending request with its payload and runs, with that payload, when another person with the permission
// approves it.
package approvals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/DrewGGM/aluna/internal/access"
	pg "github.com/DrewGGM/aluna/internal/approvals/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// Status is the state of a request. Expired is a pending one past its expiry (7 days).
type Status string

// Request states.
const (
	StatusPending  Status = "pending"
	StatusApproved Status = "approved"
	StatusRejected Status = "rejected"
	StatusFailed   Status = "failed"
	StatusExpired  Status = "expired"
)

// listLimit is the most requests List returns.
const listLimit = 100

var (
	// ErrUnknownKind is a request kind nobody registered.
	ErrUnknownKind = errors.New("unknown request kind")
	// ErrNotFound is a request that does not exist in the principal's entity.
	ErrNotFound = errors.New("request not found")
	// ErrNotPending is a request already decided.
	ErrNotPending = errors.New("the request was already decided")
	// ErrExpired is a pending request that expired.
	ErrExpired = errors.New("the request expired")
	// ErrSelfApproval is the maker trying to decide their own request (AB-5).
	ErrSelfApproval = errors.New("the maker cannot decide their own request")
	// ErrNoReason is a rejection without a reason.
	ErrNoReason = errors.New("a rejection needs a reason")
	// ErrCommandRejected is the command of an approved request that its module rejected on a business rule:
	// the Executor wraps it and the request ends up failed.
	ErrCommandRejected = errors.New("the approved command was rejected")
)

// maxFailureText is the most that is stored of a rejected command's error.
const maxFailureText = 2048

// Executor runs the command of an approved request, in the approval's transaction, with the original
// payload and the approving principal; it returns the command's result. A business rejection comes wrapped
// in ErrCommandRejected and leaves the request failed; any other error (a conflict, a timeout, a dependency
// down) is returned without deciding it, so it can be approved again. It never wraps a 40001 or a 40P01 in
// ErrCommandRejected: the Unit of Work would not see them and would not retry. Since the approval can be
// retried, it has no effects outside the transaction.
type Executor func(ctx context.Context, tx postgres.Tx, payload json.RawMessage, checker access.Principal) (result json.RawMessage, err error)

// Request is a maker-checker request.
type Request struct {
	ID, LegalEntityID    ids.ID
	Kind                 string
	Payload              json.RawMessage
	Maker, Checker       string
	Status               Status
	Reason               string
	Result               json.RawMessage
	CreatedAt, ExpiresAt time.Time
	DecidedAt            *time.Time
}

// registration is what a module registered for a request kind.
type registration struct {
	permission string
	exec       Executor
}

// Service stores and decides requests; each module registers its kinds during wiring.
type Service struct {
	policy *access.Policy
	kinds  map[string]registration
}

// New creates the service; policy says who can approve each kind.
func New(policy *access.Policy) *Service {
	return &Service{policy: policy, kinds: map[string]registration{}}
}

// RegisterKind registers kind: whoever approves or rejects it needs permission with scope all, and
// approving it runs exec. Registering a kind again replaces it.
func (s *Service) RegisterKind(kind, permission string, exec Executor) {
	s.kinds[kind] = registration{permission: permission, exec: exec}
}

// Request stores, in tx, a pending request of kind with payload, made by maker in its entity.
func (s *Service) Request(ctx context.Context, tx postgres.Tx, kind string, payload any, maker access.Principal) (Request, error) {
	if _, ok := s.kinds[kind]; !ok {
		return Request{}, fmt.Errorf("%w: %s", ErrUnknownKind, kind)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Request{}, fmt.Errorf("encode %s request payload: %w", kind, err)
	}
	row, err := pg.Insert(ctx, tx, ids.New(), maker.LegalEntityID, kind, raw, maker.Subject)
	if err != nil {
		return Request{}, err
	}
	return requestOf(row), nil
}

// Approve approves request id in tx: it runs its Executor with the original payload and, if it succeeds,
// leaves it approved with the result, in the same transaction. If the command is rejected
// (ErrCommandRejected), it undoes what it did and leaves the request failed with the error.
func (s *Service) Approve(ctx context.Context, tx postgres.Tx, id ids.ID, checker access.Principal) (Request, error) {
	row, k, err := s.lockForDecision(ctx, tx, id, checker)
	if err != nil {
		return Request{}, err
	}
	if _, err := tx.Exec(ctx, "SAVEPOINT approval_execution"); err != nil {
		return Request{}, fmt.Errorf("approval savepoint: %w", err)
	}
	result, execErr := k.exec(ctx, tx, row.Payload, checker)
	var decision pg.Decision
	switch {
	case execErr != nil && !errors.Is(execErr, ErrCommandRejected):
		return Request{}, execErr
	case execErr != nil:
		if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT approval_execution"); err != nil {
			return Request{}, fmt.Errorf("undo execution of request %s: %w", id, err)
		}
		decision = pg.Decision{Status: string(StatusFailed), Checker: checker.Subject, Error: failureText(execErr)}
	case len(result) == 0:
		decision = pg.Decision{Status: string(StatusApproved), Checker: checker.Subject, Result: json.RawMessage("null")}
	default:
		decision = pg.Decision{Status: string(StatusApproved), Checker: checker.Subject, Result: result}
	}
	decided, err := pg.Decide(ctx, tx, id, decision)
	if err != nil {
		return Request{}, err
	}
	return requestOf(decided), nil
}

// failureText is a rejected command's error as it is stored: UTF-8 text with no NUL (PostgreSQL does not
// accept them), of up to maxFailureText bytes.
func failureText(err error) string {
	text := strings.ToValidUTF8(strings.ReplaceAll(err.Error(), "\x00", ""), "")
	if len(text) > maxFailureText {
		text = strings.ToValidUTF8(text[:maxFailureText], "")
	}
	return text
}

// Reject rejects request id in tx with reason; the command does not run.
func (s *Service) Reject(ctx context.Context, tx postgres.Tx, id ids.ID, checker access.Principal, reason string) (Request, error) {
	if strings.TrimSpace(reason) == "" {
		return Request{}, ErrNoReason
	}
	if _, _, err := s.lockForDecision(ctx, tx, id, checker); err != nil {
		return Request{}, err
	}
	decided, err := pg.Decide(ctx, tx, id, pg.Decision{Status: string(StatusRejected), Checker: checker.Subject, Reason: reason})
	if err != nil {
		return Request{}, err
	}
	return requestOf(decided), nil
}

// lockForDecision locks request id for checker to decide it. It must exist in checker's entity and be of a
// kind whose permission checker holds with scope all (before looking at its state: whoever cannot decide it
// does not learn how it stands); it must also still be pending and unexpired, and not be checker's own.
func (s *Service) lockForDecision(ctx context.Context, tx postgres.Tx, id ids.ID, checker access.Principal) (pg.Row, registration, error) {
	row, found, err := pg.Lock(ctx, tx, checker.LegalEntityID, id)
	if err != nil {
		return pg.Row{}, registration{}, err
	}
	if !found {
		return pg.Row{}, registration{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	k, err := s.permitted(checker, row.Kind)
	switch {
	case err != nil:
		return pg.Row{}, registration{}, err
	case row.Status != string(StatusPending):
		return pg.Row{}, registration{}, fmt.Errorf("%w: %s is %s", ErrNotPending, id, row.Status)
	case row.Expired:
		return pg.Row{}, registration{}, fmt.Errorf("%w: %s", ErrExpired, id)
	case row.Maker == checker.Subject:
		return pg.Row{}, registration{}, ErrSelfApproval
	}
	return row, k, nil
}

// permitted is the registration of kind if checker holds its permission with scope all.
func (s *Service) permitted(checker access.Principal, kind string) (registration, error) {
	k, ok := s.kinds[kind]
	if !ok {
		return registration{}, fmt.Errorf("%w: %s", ErrUnknownKind, kind)
	}
	scope, err := s.policy.Authorize(checker, k.permission)
	if err != nil {
		return registration{}, err
	}
	if scope != access.ScopeAll {
		return registration{}, fmt.Errorf("%w: %s only with scope own", access.ErrForbidden, k.permission)
	}
	return k, nil
}

// List is the most recent requests of entity (up to 100); with status, only those in that state.
func (s *Service) List(ctx context.Context, q postgres.Querier, entity ids.ID, status Status) ([]Request, error) {
	rows, err := pg.List(ctx, q, entity, string(status), listLimit)
	if err != nil {
		return nil, err
	}
	list := make([]Request, len(rows))
	for i, row := range rows {
		list[i] = requestOf(row)
	}
	return list, nil
}

func requestOf(row pg.Row) Request {
	r := Request{
		ID: row.ID, LegalEntityID: row.LegalEntityID, Kind: row.Kind, Payload: row.Payload, Maker: row.Maker,
		Status: Status(row.Status), Result: row.Result, CreatedAt: row.CreatedAt, ExpiresAt: row.ExpiresAt,
		DecidedAt: row.DecidedAt,
	}
	if row.Expired {
		r.Status = StatusExpired
	}
	if row.Checker != nil {
		r.Checker = *row.Checker
	}
	if row.Reason != nil {
		r.Reason = *row.Reason
	}
	return r
}
