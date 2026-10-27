package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"unicode/utf8"

	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// maxIdempotencyKey es el largo máximo de Idempotency-Key, en caracteres, el mismo que acepta
// infra.idempotency_keys. El código generado no aplica el maxLength de las cabeceras.
const maxIdempotencyKey = 255

var errInvalidIdempotencyKey = fmt.Errorf("Idempotency-Key de más de %d caracteres o que no es UTF-8", maxIdempotencyKey)

// operation es lo fijo de un comando: su ruta, el status de su respuesta exitosa y los IDs que nombra la
// ruta, que su registro de auditoría lleva aunque el comando se rechace.
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

// holdOperation es una operación sobre un hold. La ruta lleva el id del hold: la huella y el scope de
// la clave son por hold, así que la misma clave con el mismo cuerpo sobre otro hold no se toma por un
// reintento.
func holdOperation(id ids.ID, action string) operation {
	return operation{method: http.MethodPost, path: "/v1/holds/" + id.String() + "/" + action, success: http.StatusOK, targets: []string{id.String()}}
}

// commands corre cada comando en su transacción, con su registro de auditoría (FR-40); los que mueven
// dinero, además, una sola vez por Idempotency-Key (architecture §9): la clave se reclama y se completa
// en la misma transacción del efecto. Las claves van por entidad: dos entidades que comparten la base no
// comparten claves.
type commands struct {
	uow    *postgres.UnitOfWork
	keys   *idempotency.Store
	entity ids.ID
}

// storedResponse es la respuesta de un comando tal como se escribe; la de uno con Idempotency-Key se
// guarda y el reintento la recibe byte a byte.
type storedResponse struct {
	status int
	body   []byte
}

// VisitCreateLedgerAccountResponse escribe la respuesta de POST /v1/ledger-accounts.
func (r storedResponse) VisitCreateLedgerAccountResponse(w http.ResponseWriter) error {
	return r.write(w)
}

// VisitPostTransactionResponse escribe la respuesta guardada de POST /v1/transactions.
func (r storedResponse) VisitPostTransactionResponse(w http.ResponseWriter) error { return r.write(w) }

// VisitPlaceHoldResponse escribe la respuesta guardada de POST /v1/holds.
func (r storedResponse) VisitPlaceHoldResponse(w http.ResponseWriter) error { return r.write(w) }

// VisitCaptureHoldResponse escribe la respuesta guardada de POST /v1/holds/{id}/capture.
func (r storedResponse) VisitCaptureHoldResponse(w http.ResponseWriter) error { return r.write(w) }

// VisitReleaseHoldResponse escribe la respuesta guardada de POST /v1/holds/{id}/release.
func (r storedResponse) VisitReleaseHoldResponse(w http.ResponseWriter) error { return r.write(w) }

// write escribe la respuesta tal como se guardó. El Store no guarda el Content-Type: lo da el status,
// porque underSavepoint solo produce el JSON del recurso (2xx) o un problem (4xx).
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

// idempotent corre effect para op una sola vez por Idempotency-Key key. Un 4xx de negocio también se
// guarda; un error que no es de negocio sube sin guardarse, para que el cliente pueda reintentar. Un
// reintento con la misma clave recibe la respuesta guardada y queda auditado como replay.
func (c commands) idempotent(ctx context.Context, op operation, key string, request any,
	effect func(tx postgres.Tx) (any, error),
) (storedResponse, error) {
	// antes de validar: un rechazo de la clave también nombra el recurso
	noteTargets(ctx, op.targets)
	if !utf8.ValidString(key) || utf8.RuneCountInString(key) > maxIdempotencyKey { // vacía no llega: la rechaza el código generado
		return storedResponse{}, errInvalidIdempotencyKey
	}
	hash, err := requestHash(op, request)
	if err != nil {
		return storedResponse{}, err
	}
	scope := c.entity.String() + " " + op.method + " " + op.path
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

// run corre effect para op, un comando sin Idempotency-Key; un rechazo de negocio se deshace hasta el
// savepoint y es la respuesta.
func (c commands) run(ctx context.Context, op operation, effect func(tx postgres.Tx) (any, error)) (storedResponse, error) {
	noteTargets(ctx, op.targets)
	return c.audited(ctx, func(tx postgres.Tx) (storedResponse, string, error) { return underSavepoint(ctx, tx, op, effect) })
}

// audited corre step en una transacción y guarda en ella el registro de auditoría del comando con la
// respuesta y el resultado que step decidió. Si la transacción no confirma, el registro se va con ella y
// lo escribe withAudit aparte.
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

// underSavepoint corre effect bajo un savepoint y lo vuelve la respuesta del comando y su resultado: ok o
// el código del problem. Un error de negocio se deshace hasta ahí, también si la base abortó la
// transacción, y su problem es la respuesta. Cualquier otro error, incluidos los conflictos que
// UnitOfWork reintenta, sube tal cual. El cuerpo termina en '\n', como el de json.Encoder que usan el
// código generado y WriteProblem.
func underSavepoint(ctx context.Context, tx postgres.Tx, op operation, effect func(tx postgres.Tx) (any, error)) (storedResponse, string, error) {
	if _, err := tx.Exec(ctx, "SAVEPOINT command_effect"); err != nil {
		return storedResponse{}, "", fmt.Errorf("savepoint del comando: %w", err)
	}
	result, effectErr := effect(tx)
	if effectErr == nil {
		body, err := json.Marshal(result)
		if err != nil {
			return storedResponse{}, "", fmt.Errorf("respuesta del comando: %w", err)
		}
		return storedResponse{status: op.success, body: append(body, '\n')}, resultOK, nil
	}
	p, isBusiness := problemFor(effectErr)
	if !isBusiness {
		return storedResponse{}, "", effectErr
	}
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT command_effect"); err != nil {
		return storedResponse{}, "", fmt.Errorf("deshacer el comando: %w", err)
	}
	body, err := json.Marshal(p.complete(ctx, op.path))
	if err != nil {
		return storedResponse{}, "", fmt.Errorf("problem del comando: %w", err)
	}
	return storedResponse{status: p.Status, body: append(body, '\n')}, p.Code, nil
}

// requestHash es la huella del request: SHA-256 del método, la ruta y el cuerpo canónico (el cuerpo
// ya leído y vuelto a serializar, así que espacios y orden de las claves no cambian la huella).
func requestHash(op operation, request any) ([32]byte, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return [32]byte{}, fmt.Errorf("huella del request: %w", err)
	}
	return sha256.Sum256(append([]byte(op.method+" "+op.path+"\n"), body...)), nil
}
