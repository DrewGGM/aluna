// Package approvals es el maker-checker transversal (FR-39, ADR-0014): un comando sensible queda como
// solicitud pendiente con su payload y se ejecuta, con ese payload, cuando otra persona con el permiso lo
// aprueba.
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

// Status es el estado de una solicitud. Expired es una pendiente que pasó su vencimiento (7 días).
type Status string

// Estados de una solicitud.
const (
	StatusPending  Status = "pending"
	StatusApproved Status = "approved"
	StatusRejected Status = "rejected"
	StatusFailed   Status = "failed"
	StatusExpired  Status = "expired"
)

// listLimit es cuántas solicitudes devuelve List como mucho.
const listLimit = 100

var (
	// ErrUnknownKind es un tipo de solicitud que nadie registró.
	ErrUnknownKind = errors.New("tipo de solicitud desconocido")
	// ErrNotFound es una solicitud que no existe en la entidad del principal.
	ErrNotFound = errors.New("solicitud no encontrada")
	// ErrNotPending es una solicitud ya decidida.
	ErrNotPending = errors.New("la solicitud ya se decidió")
	// ErrExpired es una solicitud pendiente que venció.
	ErrExpired = errors.New("la solicitud venció")
	// ErrSelfApproval es quien pidió intentando decidir su propia solicitud (AB-5).
	ErrSelfApproval = errors.New("quien pide no puede decidir su propia solicitud")
	// ErrNoReason es un rechazo sin motivo.
	ErrNoReason = errors.New("un rechazo necesita un motivo")
	// ErrCommandRejected es el comando de una solicitud aprobada que su módulo rechazó por una regla de
	// negocio: el Executor lo envuelve y la solicitud queda failed.
	ErrCommandRejected = errors.New("el comando aprobado se rechazó")
)

// maxFailureText es lo más que se guarda del error de un comando rechazado.
const maxFailureText = 2048

// Executor corre el comando de una solicitud aprobada, en la transacción de la aprobación, con el payload
// original y el principal que aprobó; devuelve el resultado del comando. Un rechazo de negocio va envuelto
// en ErrCommandRejected y deja la solicitud failed; cualquier otro error (un conflicto, un timeout, una
// dependencia caída) sube sin decidirla, para que se pueda aprobar otra vez. Nunca envuelve un 40001 o un
// 40P01 en ErrCommandRejected: la Unit of Work no los vería y no repetiría. Como la aprobación se puede
// repetir, no tiene efectos fuera de la transacción.
type Executor func(ctx context.Context, tx postgres.Tx, payload json.RawMessage, checker access.Principal) (result json.RawMessage, err error)

// Request es una solicitud de maker-checker.
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

// registration es lo que un módulo registró para un tipo de solicitud.
type registration struct {
	permission string
	exec       Executor
}

// Service guarda y decide las solicitudes; cada módulo registra sus tipos al cablear.
type Service struct {
	policy *access.Policy
	kinds  map[string]registration
}

// New crea el servicio; policy dice quién puede aprobar cada tipo.
func New(policy *access.Policy) *Service {
	return &Service{policy: policy, kinds: map[string]registration{}}
}

// RegisterKind registra el tipo kind: quien lo apruebe o rechace necesita permission con alcance all, y
// aprobarlo corre exec. Registrar de nuevo un tipo lo reemplaza.
func (s *Service) RegisterKind(kind, permission string, exec Executor) {
	s.kinds[kind] = registration{permission: permission, exec: exec}
}

// Request guarda, en tx, una solicitud pendiente de kind con payload, pedida por maker en su entidad.
func (s *Service) Request(ctx context.Context, tx postgres.Tx, kind string, payload any, maker access.Principal) (Request, error) {
	if _, ok := s.kinds[kind]; !ok {
		return Request{}, fmt.Errorf("%w: %s", ErrUnknownKind, kind)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Request{}, fmt.Errorf("payload de la solicitud %s: %w", kind, err)
	}
	row, err := pg.Insert(ctx, tx, ids.New(), maker.LegalEntityID, kind, raw, maker.Subject)
	if err != nil {
		return Request{}, err
	}
	return requestOf(row), nil
}

// Approve aprueba en tx la solicitud id: corre su Executor con el payload original y, si sale bien, la
// deja approved con el resultado, en la misma transacción. Si el comando se rechaza (ErrCommandRejected),
// deshace lo que hizo y la deja failed con el error.
func (s *Service) Approve(ctx context.Context, tx postgres.Tx, id ids.ID, checker access.Principal) (Request, error) {
	row, k, err := s.lockForDecision(ctx, tx, id, checker)
	if err != nil {
		return Request{}, err
	}
	if _, err := tx.Exec(ctx, "SAVEPOINT approval_execution"); err != nil {
		return Request{}, fmt.Errorf("savepoint de la aprobación: %w", err)
	}
	result, execErr := k.exec(ctx, tx, row.Payload, checker)
	var decision pg.Decision
	switch {
	case execErr != nil && !errors.Is(execErr, ErrCommandRejected):
		return Request{}, execErr
	case execErr != nil:
		if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT approval_execution"); err != nil {
			return Request{}, fmt.Errorf("deshacer la ejecución de la solicitud %s: %w", id, err)
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

// failureText es el error de un comando rechazado tal como se guarda: texto UTF-8 sin NUL (PostgreSQL no
// los acepta), de hasta maxFailureText bytes.
func failureText(err error) string {
	text := strings.ToValidUTF8(strings.ReplaceAll(err.Error(), "\x00", ""), "")
	if len(text) > maxFailureText {
		text = strings.ToValidUTF8(text[:maxFailureText], "")
	}
	return text
}

// Reject rechaza en tx la solicitud id con reason; el comando no corre.
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

// lockForDecision bloquea la solicitud id para que checker la decida. Tiene que existir en su entidad y ser
// de un tipo cuyo permiso checker tenga con alcance all (antes de mirar su estado: quien no puede decidirla
// no se entera de cómo está); además, seguir pendiente y sin vencer, y no ser suya.
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
		return pg.Row{}, registration{}, fmt.Errorf("%w: %s está %s", ErrNotPending, id, row.Status)
	case row.Expired:
		return pg.Row{}, registration{}, fmt.Errorf("%w: %s", ErrExpired, id)
	case row.Maker == checker.Subject:
		return pg.Row{}, registration{}, ErrSelfApproval
	}
	return row, k, nil
}

// permitted es el registro del tipo kind si checker tiene su permiso con alcance all.
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
		return registration{}, fmt.Errorf("%w: %s solo con alcance own", access.ErrForbidden, k.permission)
	}
	return k, nil
}

// List son las solicitudes más recientes de la entidad entity (hasta 100); con status, solo las de ese
// estado.
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
