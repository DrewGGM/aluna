//go:build integration

package approvals_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DrewGGM/aluna/internal/access"
	"github.com/DrewGGM/aluna/internal/approvals"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, &testPool)) }

const (
	testKind       = "prueba.ajuste"
	testPermission = "prueba.ajustes.aprobar"
)

var testPolicy = func() *access.Policy {
	p, err := access.ParsePolicy([]byte("operator:\n  all: [" + testPermission + "]\nchannel:\n  own: [" + testPermission + "]\n"))
	if err != nil {
		panic(err)
	}
	return p
}()

// world es una entidad con dos operadores, el que pide y el que aprueba, y un servicio cuyo único tipo
// de solicitud corre exec.
type world struct {
	svc            *approvals.Service
	maker, checker access.Principal
}

func newWorld(exec approvals.Executor) world {
	entity := ids.New()
	svc := approvals.New(testPolicy)
	svc.RegisterKind(testKind, testPermission, exec)
	return world{
		svc:     svc,
		maker:   access.Principal{Subject: "operador-a", LegalEntityID: entity, Roles: []string{"operator"}},
		checker: access.Principal{Subject: "operador-b", LegalEntityID: entity, Roles: []string{"operator"}},
	}
}

func inTx[T any](t *testing.T, fn func(tx postgres.Tx) (T, error)) (T, error) { //nolint:ireturn // T es lo que devuelve fn
	t.Helper()
	var out T
	err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		var err error
		out, err = fn(tx)
		return err
	})
	return out, err
}

func (w world) request(t *testing.T, payload any) approvals.Request {
	t.Helper()
	r, err := inTx(t, func(tx postgres.Tx) (approvals.Request, error) {
		return w.svc.Request(t.Context(), tx, testKind, payload, w.maker)
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (w world) approve(t *testing.T, id ids.ID, checker access.Principal) (approvals.Request, error) {
	t.Helper()
	return inTx(t, func(tx postgres.Tx) (approvals.Request, error) { return w.svc.Approve(t.Context(), tx, id, checker) })
}

func (w world) reject(t *testing.T, id ids.ID, checker access.Principal, reason string) (approvals.Request, error) {
	t.Helper()
	return inTx(t, func(tx postgres.Tx) (approvals.Request, error) {
		return w.svc.Reject(t.Context(), tx, id, checker, reason)
	})
}

// TestApproveRunsTheCommandWithTheOriginalPayload (FR-39): aprobar corre el comando una vez, con el payload
// que se pidió y el principal que aprueba, y la solicitud queda approved con su resultado.
func TestApproveRunsTheCommandWithTheOriginalPayload(t *testing.T) {
	var gotPayload string
	var gotChecker access.Principal
	runs := 0
	w := newWorld(func(_ context.Context, _ postgres.Tx, payload json.RawMessage, checker access.Principal) (json.RawMessage, error) {
		runs++
		gotPayload, gotChecker = string(payload), checker
		return json.RawMessage(`{"asentado": true}`), nil
	})
	r := w.request(t, map[string]any{"monto": 9_007_199_254_740_993})

	approved, err := w.approve(t, r.ID, w.checker)

	if err != nil || runs != 1 || gotPayload != `{"monto": 9007199254740993}` || gotChecker.Subject != "operador-b" {
		t.Fatalf("= %v; corrió %d veces con %s y %+v", err, runs, gotPayload, gotChecker)
	}
	if approved.Status != approvals.StatusApproved || approved.Checker != "operador-b" || approved.Maker != "operador-a" ||
		string(approved.Result) != `{"asentado": true}` || approved.DecidedAt == nil {
		t.Fatalf("solicitud aprobada = %+v", approved)
	}
}

// TestACommandWithoutResult: un comando que no devuelve resultado, nil o vacío, deja la solicitud approved
// con resultado null.
func TestACommandWithoutResult(t *testing.T) {
	for name, result := range map[string]json.RawMessage{"nil": nil, "vacío": {}} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(func(context.Context, postgres.Tx, json.RawMessage, access.Principal) (json.RawMessage, error) {
				return result, nil
			})
			r := w.request(t, map[string]any{})

			approved, err := w.approve(t, r.ID, w.checker)

			if err != nil || approved.Status != approvals.StatusApproved || string(approved.Result) != "null" {
				t.Fatalf("= %+v, %v; se esperaba approved con resultado null", approved, err)
			}
		})
	}
}

// TestALongRejectionIsTruncated: del error de un comando rechazado se guardan hasta 2 KB.
func TestALongRejectionIsTruncated(t *testing.T) {
	w := newWorld(func(context.Context, postgres.Tx, json.RawMessage, access.Principal) (json.RawMessage, error) {
		return nil, fmt.Errorf("%w: %s", approvals.ErrCommandRejected, strings.Repeat("á", 3000))
	})
	r := w.request(t, map[string]any{})

	if _, err := w.approve(t, r.ID, w.checker); err != nil {
		t.Fatal(err)
	}

	var length int
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `SELECT octet_length(error) FROM approvals.requests WHERE id = $1`, r.ID).Scan(&length)
	})
	if length == 0 || length > 2048 {
		t.Fatalf("error guardado de %d bytes, se esperaba hasta 2048", length)
	}
}

// TestARequestExpiresInSevenDays: una solicitud pendiente vence a los 7 días de creada.
func TestARequestExpiresInSevenDays(t *testing.T) {
	r := newWorld(succeed).request(t, map[string]any{})

	if life := r.ExpiresAt.Sub(r.CreatedAt); life != 7*24*time.Hour || r.Status != approvals.StatusPending {
		t.Fatalf("la solicitud %s vence a los %s de creada, se esperaba pendiente por 7 días", r.Status, life)
	}
}

// TestTheMakerCannotDecide (AB-5, TH-4): quien pidió no aprueba ni rechaza su solicitud, y la base lo
// impide aunque el servicio no lo revisara.
func TestTheMakerCannotDecide(t *testing.T) {
	w := newWorld(succeed)
	r := w.request(t, map[string]any{})

	_, approveErr := w.approve(t, r.ID, w.maker)
	_, rejectErr := w.reject(t, r.ID, w.maker, "no")

	if !errors.Is(approveErr, approvals.ErrSelfApproval) || !errors.Is(rejectErr, approvals.ErrSelfApproval) {
		t.Fatalf("aprobar = %v, rechazar = %v; se esperaba ErrSelfApproval", approveErr, rejectErr)
	}
	_, err := inTx(t, func(tx postgres.Tx) (int64, error) {
		tag, err := tx.Exec(t.Context(), `UPDATE approvals.requests SET status = 'rejected', checker = maker, reason = 'x',
			decided_at = now() WHERE id = $1`, r.ID)
		return tag.RowsAffected(), err
	})
	if code := pgCode(err); code != "23514" {
		t.Fatalf("checker = maker por SQL = %v, se esperaba el CHECK (23514)", err)
	}
}

func succeed(context.Context, postgres.Tx, json.RawMessage, access.Principal) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// TestARejectedCommandLeavesTheRequestFailed: si el comando se rechaza por una regla de negocio, lo que
// alcanzó a hacer se deshace y la solicitud queda failed con el error; no se puede volver a aprobar.
func TestARejectedCommandLeavesTheRequestFailed(t *testing.T) {
	var w world
	w = newWorld(func(ctx context.Context, tx postgres.Tx, _ json.RawMessage, checker access.Principal) (json.RawMessage, error) {
		if _, err := w.svc.Request(ctx, tx, testKind, map[string]any{"efecto": "parcial"}, checker); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: fondos insuficientes\x00", approvals.ErrCommandRejected)
	})
	r := w.request(t, map[string]any{})

	failed, err := w.approve(t, r.ID, w.checker)

	if err != nil || failed.Status != approvals.StatusFailed || failed.Checker != "operador-b" {
		t.Fatalf("= %+v, %v; se esperaba failed", failed, err)
	}
	var stored string
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `SELECT error FROM approvals.requests WHERE id = $1`, r.ID).Scan(&stored)
	})
	if stored != "el comando aprobado se rechazó: fondos insuficientes" {
		t.Fatalf("error guardado = %q, se esperaba el del comando", stored)
	}
	list, err := w.svc.List(t.Context(), testPool, w.maker.LegalEntityID, "")
	if err != nil || len(list) != 1 {
		t.Fatalf("solicitudes de la entidad = %d (%v); el efecto del comando fallido debía deshacerse", len(list), err)
	}
	if _, err := w.approve(t, r.ID, w.checker); !errors.Is(err, approvals.ErrNotPending) {
		t.Fatalf("aprobar una failed = %v, se esperaba ErrNotPending", err)
	}
}

// TestAnUnexpectedErrorKeepsTheRequestPending: un error del comando que no es un rechazo de negocio (un
// timeout, una dependencia caída) sube sin decidir la solicitud, que se puede aprobar después.
func TestAnUnexpectedErrorKeepsTheRequestPending(t *testing.T) {
	down := errors.New("la dependencia no responde")
	fail := true
	w := newWorld(func(context.Context, postgres.Tx, json.RawMessage, access.Principal) (json.RawMessage, error) {
		if fail {
			return nil, down
		}
		return json.RawMessage(`{}`), nil
	})
	r := w.request(t, map[string]any{})

	_, err := w.approve(t, r.ID, w.checker)
	pending, listErr := w.svc.List(t.Context(), testPool, w.maker.LegalEntityID, approvals.StatusPending)
	fail = false
	approved, retryErr := w.approve(t, r.ID, w.checker)

	if !errors.Is(err, down) || listErr != nil || len(pending) != 1 {
		t.Fatalf("= %v; pendientes %d (%v); se esperaba el error y la solicitud pendiente", err, len(pending), listErr)
	}
	if retryErr != nil || approved.Status != approvals.StatusApproved {
		t.Fatalf("aprobar de nuevo = %+v, %v; se esperaba approved", approved, retryErr)
	}
}

// TestAConflictIsRetriedNotFailed: un conflicto que la Unit of Work repite no deja la solicitud failed: se
// repite la aprobación entera y, si el conflicto pasa, queda approved.
func TestAConflictIsRetriedNotFailed(t *testing.T) {
	attempts := 0
	w := newWorld(func(context.Context, postgres.Tx, json.RawMessage, access.Principal) (json.RawMessage, error) {
		attempts++
		if attempts == 1 {
			return nil, &pgconn.PgError{Code: "40001"}
		}
		return json.RawMessage(`{}`), nil
	})
	r := w.request(t, map[string]any{})

	approved, err := w.approve(t, r.ID, w.checker)

	if err != nil || approved.Status != approvals.StatusApproved || attempts != 2 {
		t.Fatalf("= %+v, %v tras %d intentos; se esperaba approved al segundo", approved, err, attempts)
	}
}

// TestADecidedRequestDoesNotChange: aprobar o rechazar dos veces da ErrNotPending, y la base no deja
// cambiar ni borrar una solicitud decidida, ni lo que se pidió en una pendiente.
func TestADecidedRequestDoesNotChange(t *testing.T) {
	w := newWorld(succeed)
	decided := w.request(t, map[string]any{})
	if _, err := w.reject(t, decided.ID, w.checker, "no corresponde"); err != nil {
		t.Fatal(err)
	}
	pending := w.request(t, map[string]any{})

	_, approveErr := w.approve(t, decided.ID, w.checker)
	_, rejectErr := w.reject(t, decided.ID, w.checker, "otra vez")

	if !errors.Is(approveErr, approvals.ErrNotPending) || !errors.Is(rejectErr, approvals.ErrNotPending) {
		t.Fatalf("aprobar = %v, rechazar = %v; se esperaba ErrNotPending", approveErr, rejectErr)
	}
	for name, sql := range map[string]string{
		"cambiar una decidida":                `UPDATE approvals.requests SET reason = 'otro' WHERE id = '` + decided.ID.String() + `'`,
		"borrar una decidida":                 `DELETE FROM approvals.requests WHERE id = '` + decided.ID.String() + `'`,
		"cambiar el payload de una pendiente": `UPDATE approvals.requests SET payload = '{"x": 1}' WHERE id = '` + pending.ID.String() + `'`,
		"truncar":                             `TRUNCATE approvals.requests`,
	} {
		err := ownerExec(t, sql)
		if pgCode(err) != "2F003" {
			t.Errorf("%s como dueño = %v, se esperaba que la base lo impidiera", name, err)
		}
	}
}

// ownerExec corre sql como dueño de las tablas, en un savepoint que siempre se deshace, y devuelve su error.
func ownerExec(t *testing.T, sql string) error {
	t.Helper()
	var execErr error
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		sp, err := tx.Begin(t.Context())
		if err != nil {
			return err
		}
		_, execErr = sp.Exec(t.Context(), sql)
		return sp.Rollback(t.Context())
	})
	return execErr
}

// TestAnExpiredRequestCannotBeDecided: a los 7 días una pendiente vence: no se aprueba ni se rechaza, ni
// siquiera por SQL, y List la muestra como expired.
func TestAnExpiredRequestCannotBeDecided(t *testing.T) {
	w := newWorld(succeed)
	expired := ids.New()
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO approvals.requests (id, legal_entity_id, kind, payload, maker, created_at, expires_at)
			VALUES ($1, $2, $3, '{}', $4, now() - interval '8 days', now() - interval '1 day')`,
			expired, w.maker.LegalEntityID, testKind, w.maker.Subject)
		return err
	})

	_, approveErr := w.approve(t, expired, w.checker)
	_, rejectErr := w.reject(t, expired, w.checker, "tarde")
	list, err := w.svc.List(t.Context(), testPool, w.maker.LegalEntityID, approvals.StatusExpired)

	if !errors.Is(approveErr, approvals.ErrExpired) || !errors.Is(rejectErr, approvals.ErrExpired) {
		t.Fatalf("aprobar = %v, rechazar = %v; se esperaba ErrExpired", approveErr, rejectErr)
	}
	if err != nil || len(list) != 1 || list[0].ID != expired || list[0].Status != approvals.StatusExpired {
		t.Fatalf("vencidas = %+v (%v), se esperaba solo %s", list, err, expired)
	}
	sql := `UPDATE approvals.requests SET status = 'rejected', checker = 'operador-b', reason = 'x', decided_at = now()
		WHERE id = '` + expired.String() + `'`
	if err := ownerExec(t, sql); pgCode(err) != "2F003" {
		t.Fatalf("decidir por SQL una vencida = %v, se esperaba que la base lo impidiera", err)
	}
}

// TestOnlyACheckerWithThePermissionDecides: aprueba quien tiene el permiso del tipo con alcance all, de la
// misma entidad; un tipo que nadie registró no se pide.
func TestOnlyACheckerWithThePermissionDecides(t *testing.T) {
	w := newWorld(succeed)
	r := w.request(t, map[string]any{})
	cases := map[string]struct {
		checker access.Principal
		want    error
	}{
		"sin el permiso":   {access.Principal{Subject: "auditor", LegalEntityID: w.maker.LegalEntityID, Roles: []string{"auditor"}}, access.ErrForbidden},
		"solo alcance own": {access.Principal{Subject: "canal", LegalEntityID: w.maker.LegalEntityID, Roles: []string{"channel"}}, access.ErrForbidden},
		"de otra entidad":  {access.Principal{Subject: "operador-x", LegalEntityID: ids.New(), Roles: []string{"operator"}}, approvals.ErrNotFound},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := w.approve(t, r.ID, tc.checker); !errors.Is(err, tc.want) {
				t.Fatalf("= %v, se esperaba %v", err, tc.want)
			}
		})
	}
	_, err := inTx(t, func(tx postgres.Tx) (approvals.Request, error) {
		return w.svc.Request(t.Context(), tx, "otro.tipo", map[string]any{}, w.maker)
	})
	if !errors.Is(err, approvals.ErrUnknownKind) {
		t.Fatalf("pedir un tipo sin registrar = %v, se esperaba ErrUnknownKind", err)
	}
}

// TestRejectionNeedsAReason: un rechazo sin motivo no se guarda; con motivo, la solicitud queda rejected
// y el comando no corre.
func TestRejectionNeedsAReason(t *testing.T) {
	runs := 0
	w := newWorld(func(context.Context, postgres.Tx, json.RawMessage, access.Principal) (json.RawMessage, error) {
		runs++
		return nil, nil
	})
	r := w.request(t, map[string]any{})

	_, noReason := w.reject(t, r.ID, w.checker, "   ")
	rejected, err := w.reject(t, r.ID, w.checker, "monto equivocado")

	if !errors.Is(noReason, approvals.ErrNoReason) {
		t.Fatalf("sin motivo = %v, se esperaba ErrNoReason", noReason)
	}
	if err != nil || rejected.Status != approvals.StatusRejected || rejected.Reason != "monto equivocado" || runs != 0 {
		t.Fatalf("= %+v, %v; el comando corrió %d veces", rejected, err, runs)
	}
}

// TestListFiltersByStatusAndEntity: List solo ve la entidad pedida, de la más nueva a la más vieja, y
// filtra por estado.
func TestListFiltersByStatusAndEntity(t *testing.T) {
	w := newWorld(succeed)
	first, second := w.request(t, map[string]any{}), w.request(t, map[string]any{})
	if _, err := w.approve(t, first.ID, w.checker); err != nil {
		t.Fatal(err)
	}
	newWorld(succeed).request(t, map[string]any{}) // de otra entidad

	all, errAll := w.svc.List(t.Context(), testPool, w.maker.LegalEntityID, "")
	pending, errPending := w.svc.List(t.Context(), testPool, w.maker.LegalEntityID, approvals.StatusPending)

	idsOf := func(list []approvals.Request) []ids.ID {
		out := []ids.ID{}
		for _, r := range list {
			out = append(out, r.ID)
		}
		return out
	}
	if errAll != nil || !slices.Equal(idsOf(all), []ids.ID{second.ID, first.ID}) {
		t.Fatalf("todas = %v (%v), se esperaba [%s %s]", idsOf(all), errAll, second.ID, first.ID)
	}
	if errPending != nil || !slices.Equal(idsOf(pending), []ids.ID{second.ID}) {
		t.Fatalf("pendientes = %v (%v), se esperaba [%s]", idsOf(pending), errPending, second.ID)
	}
}
