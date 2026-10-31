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
	testKind       = "test.adjustment"
	testPermission = "test.adjustments.approve"
)

var testPolicy = func() *access.Policy {
	p, err := access.ParsePolicy([]byte("operator:\n  all: [" + testPermission + "]\nchannel:\n  own: [" + testPermission + "]\n"))
	if err != nil {
		panic(err)
	}
	return p
}()

// world is an entity with two operators, the maker and the checker, and a service whose only request kind
// runs exec.
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
		maker:   access.Principal{Subject: "operator-a", LegalEntityID: entity, Roles: []string{"operator"}},
		checker: access.Principal{Subject: "operator-b", LegalEntityID: entity, Roles: []string{"operator"}},
	}
}

func inTx[T any](t *testing.T, fn func(tx postgres.Tx) (T, error)) (T, error) { //nolint:ireturn // T is what fn returns
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

// TestApproveRunsTheCommandWithTheOriginalPayload (FR-39): approving runs the command once, with the payload
// that was requested and the approving principal, and the request ends up approved with its result.
func TestApproveRunsTheCommandWithTheOriginalPayload(t *testing.T) {
	var gotPayload string
	var gotChecker access.Principal
	runs := 0
	w := newWorld(func(_ context.Context, _ postgres.Tx, payload json.RawMessage, checker access.Principal) (json.RawMessage, error) {
		runs++
		gotPayload, gotChecker = string(payload), checker
		return json.RawMessage(`{"posted": true}`), nil
	})
	r := w.request(t, map[string]any{"amount": 9_007_199_254_740_993})

	approved, err := w.approve(t, r.ID, w.checker)

	if err != nil || runs != 1 || gotPayload != `{"amount": 9007199254740993}` || gotChecker.Subject != "operator-b" {
		t.Fatalf("= %v; ran %d times with %s and %+v", err, runs, gotPayload, gotChecker)
	}
	if approved.Status != approvals.StatusApproved || approved.Checker != "operator-b" || approved.Maker != "operator-a" ||
		string(approved.Result) != `{"posted": true}` || approved.DecidedAt == nil {
		t.Fatalf("approved request = %+v", approved)
	}
}

// TestACommandWithoutResult: a command that returns no result, nil or empty, leaves the request approved
// with a null result.
func TestACommandWithoutResult(t *testing.T) {
	for name, result := range map[string]json.RawMessage{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(func(context.Context, postgres.Tx, json.RawMessage, access.Principal) (json.RawMessage, error) {
				return result, nil
			})
			r := w.request(t, map[string]any{})

			approved, err := w.approve(t, r.ID, w.checker)

			if err != nil || approved.Status != approvals.StatusApproved || string(approved.Result) != "null" {
				t.Fatalf("= %+v, %v; want approved with a null result", approved, err)
			}
		})
	}
}

// TestALongRejectionIsTruncated: up to 2 KB of a rejected command's error is stored.
func TestALongRejectionIsTruncated(t *testing.T) {
	w := newWorld(func(context.Context, postgres.Tx, json.RawMessage, access.Principal) (json.RawMessage, error) {
		return nil, fmt.Errorf("%w: %s", approvals.ErrCommandRejected, strings.Repeat("€", 3000))
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
		t.Fatalf("stored error of %d bytes, want up to 2048", length)
	}
}

// TestARequestExpiresInSevenDays: a pending request expires 7 days after it is created.
func TestARequestExpiresInSevenDays(t *testing.T) {
	r := newWorld(succeed).request(t, map[string]any{})

	if life := r.ExpiresAt.Sub(r.CreatedAt); life != 7*24*time.Hour || r.Status != approvals.StatusPending {
		t.Fatalf("the %s request expires %s after creation, want pending for 7 days", r.Status, life)
	}
}

// TestTheMakerCannotDecide (AB-5, TH-4): the maker neither approves nor rejects their request, and the
// database prevents it even if the service did not check.
func TestTheMakerCannotDecide(t *testing.T) {
	w := newWorld(succeed)
	r := w.request(t, map[string]any{})

	_, approveErr := w.approve(t, r.ID, w.maker)
	_, rejectErr := w.reject(t, r.ID, w.maker, "no")

	if !errors.Is(approveErr, approvals.ErrSelfApproval) || !errors.Is(rejectErr, approvals.ErrSelfApproval) {
		t.Fatalf("approve = %v, reject = %v; want ErrSelfApproval", approveErr, rejectErr)
	}
	_, err := inTx(t, func(tx postgres.Tx) (int64, error) {
		tag, err := tx.Exec(t.Context(), `UPDATE approvals.requests SET status = 'rejected', checker = maker, reason = 'x',
			decided_at = now() WHERE id = $1`, r.ID)
		return tag.RowsAffected(), err
	})
	if code := pgCode(err); code != "23514" {
		t.Fatalf("checker = maker via SQL = %v, want the CHECK (23514)", err)
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

// TestARejectedCommandLeavesTheRequestFailed: if the command is rejected on a business rule, whatever it
// managed to do is undone and the request ends up failed with the error; it cannot be approved again.
func TestARejectedCommandLeavesTheRequestFailed(t *testing.T) {
	var w world
	w = newWorld(func(ctx context.Context, tx postgres.Tx, _ json.RawMessage, checker access.Principal) (json.RawMessage, error) {
		if _, err := w.svc.Request(ctx, tx, testKind, map[string]any{"effect": "partial"}, checker); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: insufficient funds\x00", approvals.ErrCommandRejected)
	})
	r := w.request(t, map[string]any{})

	failed, err := w.approve(t, r.ID, w.checker)

	if err != nil || failed.Status != approvals.StatusFailed || failed.Checker != "operator-b" {
		t.Fatalf("= %+v, %v; want failed", failed, err)
	}
	var stored string
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `SELECT error FROM approvals.requests WHERE id = $1`, r.ID).Scan(&stored)
	})
	if stored != "the approved command was rejected: insufficient funds" {
		t.Fatalf("stored error = %q, want the command's", stored)
	}
	list, err := w.svc.List(t.Context(), testPool, w.maker.LegalEntityID, "")
	if err != nil || len(list) != 1 {
		t.Fatalf("entity requests = %d (%v); the failed command's effect should have been undone", len(list), err)
	}
	if _, err := w.approve(t, r.ID, w.checker); !errors.Is(err, approvals.ErrNotPending) {
		t.Fatalf("approving a failed one = %v, want ErrNotPending", err)
	}
}

// TestAnUnexpectedErrorKeepsTheRequestPending: a command error that is not a business rejection (a timeout,
// a dependency down) is returned without deciding the request, which can be approved later.
func TestAnUnexpectedErrorKeepsTheRequestPending(t *testing.T) {
	down := errors.New("the dependency does not respond")
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
		t.Fatalf("= %v; pending %d (%v); want the error and the request pending", err, len(pending), listErr)
	}
	if retryErr != nil || approved.Status != approvals.StatusApproved {
		t.Fatalf("approving again = %+v, %v; want approved", approved, retryErr)
	}
}

// TestAConflictIsRetriedNotFailed: a conflict that the Unit of Work retries does not leave the request
// failed: the whole approval is retried and, once the conflict clears, it ends up approved.
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
		t.Fatalf("= %+v, %v after %d attempts; want approved on the second", approved, err, attempts)
	}
}

// TestADecidedRequestDoesNotChange: approving or rejecting twice gives ErrNotPending, and the database does
// not allow changing or deleting a decided request, nor what was requested in a pending one.
func TestADecidedRequestDoesNotChange(t *testing.T) {
	w := newWorld(succeed)
	decided := w.request(t, map[string]any{})
	if _, err := w.reject(t, decided.ID, w.checker, "not applicable"); err != nil {
		t.Fatal(err)
	}
	pending := w.request(t, map[string]any{})

	_, approveErr := w.approve(t, decided.ID, w.checker)
	_, rejectErr := w.reject(t, decided.ID, w.checker, "once more")

	if !errors.Is(approveErr, approvals.ErrNotPending) || !errors.Is(rejectErr, approvals.ErrNotPending) {
		t.Fatalf("approve = %v, reject = %v; want ErrNotPending", approveErr, rejectErr)
	}
	for name, sql := range map[string]string{
		"change a decided one":                `UPDATE approvals.requests SET reason = 'other' WHERE id = '` + decided.ID.String() + `'`,
		"delete a decided one":                `DELETE FROM approvals.requests WHERE id = '` + decided.ID.String() + `'`,
		"change the payload of a pending one": `UPDATE approvals.requests SET payload = '{"x": 1}' WHERE id = '` + pending.ID.String() + `'`,
		"truncate":                            `TRUNCATE approvals.requests`,
	} {
		err := ownerExec(t, sql)
		if pgCode(err) != "2F003" {
			t.Errorf("%s as owner = %v, want the database to prevent it", name, err)
		}
	}
}

// ownerExec runs sql as the table owner, in a savepoint that is always rolled back, and returns its error.
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

// TestAnExpiredRequestCannotBeDecided: after 7 days a pending request expires: it is neither approved nor
// rejected, not even via SQL, and List shows it as expired.
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
	_, rejectErr := w.reject(t, expired, w.checker, "too late")
	list, err := w.svc.List(t.Context(), testPool, w.maker.LegalEntityID, approvals.StatusExpired)

	if !errors.Is(approveErr, approvals.ErrExpired) || !errors.Is(rejectErr, approvals.ErrExpired) {
		t.Fatalf("approve = %v, reject = %v; want ErrExpired", approveErr, rejectErr)
	}
	if err != nil || len(list) != 1 || list[0].ID != expired || list[0].Status != approvals.StatusExpired {
		t.Fatalf("expired = %+v (%v), want only %s", list, err, expired)
	}
	sql := `UPDATE approvals.requests SET status = 'rejected', checker = 'operator-b', reason = 'x', decided_at = now()
		WHERE id = '` + expired.String() + `'`
	if err := ownerExec(t, sql); pgCode(err) != "2F003" {
		t.Fatalf("deciding an expired one via SQL = %v, want the database to prevent it", err)
	}
}

// TestOnlyACheckerWithThePermissionDecides: whoever holds the kind's permission with scope all, in the same
// entity, approves; a kind nobody registered cannot be requested.
func TestOnlyACheckerWithThePermissionDecides(t *testing.T) {
	w := newWorld(succeed)
	r := w.request(t, map[string]any{})
	cases := map[string]struct {
		checker access.Principal
		want    error
	}{
		"without the permission": {access.Principal{Subject: "auditor", LegalEntityID: w.maker.LegalEntityID, Roles: []string{"auditor"}}, access.ErrForbidden},
		"scope own only":         {access.Principal{Subject: "channel", LegalEntityID: w.maker.LegalEntityID, Roles: []string{"channel"}}, access.ErrForbidden},
		"from another entity":    {access.Principal{Subject: "operator-x", LegalEntityID: ids.New(), Roles: []string{"operator"}}, approvals.ErrNotFound},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := w.approve(t, r.ID, tc.checker); !errors.Is(err, tc.want) {
				t.Fatalf("= %v, want %v", err, tc.want)
			}
		})
	}
	_, err := inTx(t, func(tx postgres.Tx) (approvals.Request, error) {
		return w.svc.Request(t.Context(), tx, "other.kind", map[string]any{}, w.maker)
	})
	if !errors.Is(err, approvals.ErrUnknownKind) {
		t.Fatalf("requesting an unregistered kind = %v, want ErrUnknownKind", err)
	}
}

// TestRejectionNeedsAReason: a rejection without a reason is not stored; with a reason, the request ends up
// rejected and the command does not run.
func TestRejectionNeedsAReason(t *testing.T) {
	runs := 0
	w := newWorld(func(context.Context, postgres.Tx, json.RawMessage, access.Principal) (json.RawMessage, error) {
		runs++
		return nil, nil
	})
	r := w.request(t, map[string]any{})

	_, noReason := w.reject(t, r.ID, w.checker, "   ")
	rejected, err := w.reject(t, r.ID, w.checker, "wrong amount")

	if !errors.Is(noReason, approvals.ErrNoReason) {
		t.Fatalf("without a reason = %v, want ErrNoReason", noReason)
	}
	if err != nil || rejected.Status != approvals.StatusRejected || rejected.Reason != "wrong amount" || runs != 0 {
		t.Fatalf("= %+v, %v; the command ran %d times", rejected, err, runs)
	}
}

// TestListFiltersByStatusAndEntity: List only sees the requested entity, newest first, and filters by
// state.
func TestListFiltersByStatusAndEntity(t *testing.T) {
	w := newWorld(succeed)
	first, second := w.request(t, map[string]any{}), w.request(t, map[string]any{})
	if _, err := w.approve(t, first.ID, w.checker); err != nil {
		t.Fatal(err)
	}
	newWorld(succeed).request(t, map[string]any{}) // from another entity

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
		t.Fatalf("all = %v (%v), want [%s %s]", idsOf(all), errAll, second.ID, first.ID)
	}
	if errPending != nil || !slices.Equal(idsOf(pending), []ids.ID{second.ID}) {
		t.Fatalf("pending = %v (%v), want [%s]", idsOf(pending), errPending, second.ID)
	}
}
