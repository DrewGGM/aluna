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

// idempotentOperation es lo fijo de un comando idempotente: su ruta y el status de su respuesta exitosa.
type idempotentOperation struct {
	method, path string
	success      int
}

var postTransaction = idempotentOperation{method: http.MethodPost, path: "/v1/transactions", success: http.StatusCreated}

// idempotentCommands corre los comandos que mueven dinero una sola vez por Idempotency-Key
// (architecture §9): la clave se reclama y se completa en la misma transacción del efecto. Las claves
// van por entidad: dos entidades que comparten la base no comparten claves.
type idempotentCommands struct {
	uow    *postgres.UnitOfWork
	keys   *idempotency.Store
	entity ids.ID
}

// storedResponse es la respuesta de un comando idempotente tal como se guarda: el reintento la recibe
// byte a byte.
type storedResponse struct {
	status int
	body   []byte
}

// VisitPostTransactionResponse escribe la respuesta guardada de POST /v1/transactions. El Store no
// guarda el Content-Type: lo da el status, porque underSavepoint solo produce el JSON del recurso
// (2xx) o un problem (4xx).
func (r storedResponse) VisitPostTransactionResponse(w http.ResponseWriter) error {
	contentType := "application/json"
	if r.status >= http.StatusBadRequest {
		contentType = "application/problem+json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(r.status)
	_, err := w.Write(r.body)
	return err
}

// run corre effect para op con la Idempotency-Key key. Un 4xx de negocio también se guarda; un error
// que no es de negocio sube sin guardarse, para que el cliente pueda reintentar.
func (c idempotentCommands) run(ctx context.Context, op idempotentOperation, key string, request any,
	effect func(tx postgres.Tx) (any, error),
) (storedResponse, error) {
	if !utf8.ValidString(key) || utf8.RuneCountInString(key) > maxIdempotencyKey { // vacía no llega: la rechaza el código generado
		return storedResponse{}, errInvalidIdempotencyKey
	}
	hash, err := requestHash(op, request)
	if err != nil {
		return storedResponse{}, err
	}
	scope := c.entity.String() + " " + op.method + " " + op.path
	var resp storedResponse
	err = c.uow.Do(ctx, func(tx postgres.Tx) error {
		claim, err := c.keys.Claim(ctx, tx, scope, key, hash)
		if err != nil {
			return err
		}
		if !claim.Fresh {
			resp = storedResponse{status: claim.Status, body: claim.Body}
			return nil
		}
		if resp, err = underSavepoint(ctx, tx, op, effect); err != nil {
			return err
		}
		return c.keys.Complete(ctx, tx, scope, key, resp.status, resp.body)
	})
	return resp, err
}

// underSavepoint corre effect bajo un savepoint y lo vuelve la respuesta que se guarda. Un error de
// negocio se deshace hasta ahí, también si la base abortó la transacción (un 23505), y su problem es la
// respuesta. Cualquier otro error, incluidos los conflictos que UnitOfWork reintenta, sube tal cual. El
// cuerpo termina en '\n', como el de json.Encoder que usan el código generado y WriteProblem.
func underSavepoint(ctx context.Context, tx postgres.Tx, op idempotentOperation, effect func(tx postgres.Tx) (any, error)) (storedResponse, error) {
	if _, err := tx.Exec(ctx, "SAVEPOINT idempotent_effect"); err != nil {
		return storedResponse{}, fmt.Errorf("savepoint del comando idempotente: %w", err)
	}
	result, effectErr := effect(tx)
	if effectErr == nil {
		body, err := json.Marshal(result)
		if err != nil {
			return storedResponse{}, fmt.Errorf("respuesta del comando idempotente: %w", err)
		}
		return storedResponse{status: op.success, body: append(body, '\n')}, nil
	}
	p, isBusiness := problemFor(effectErr)
	if !isBusiness {
		return storedResponse{}, effectErr
	}
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT idempotent_effect"); err != nil {
		return storedResponse{}, fmt.Errorf("deshacer el comando idempotente: %w", err)
	}
	body, err := json.Marshal(p.complete(ctx, op.path))
	if err != nil {
		return storedResponse{}, fmt.Errorf("problem del comando idempotente: %w", err)
	}
	return storedResponse{status: p.Status, body: append(body, '\n')}, nil
}

// requestHash es la huella del request: SHA-256 del método, la ruta y el cuerpo canónico (el cuerpo
// ya leído y vuelto a serializar, así que espacios y orden de las claves no cambian la huella).
func requestHash(op idempotentOperation, request any) ([32]byte, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return [32]byte{}, fmt.Errorf("huella del request: %w", err)
	}
	return sha256.Sum256(append([]byte(op.method+" "+op.path+"\n"), body...)), nil
}
