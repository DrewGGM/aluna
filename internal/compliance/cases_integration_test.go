//go:build integration

package compliance_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DrewGGM/aluna/internal/compliance"
	"github.com/DrewGGM/aluna/internal/compliance/internal/adapters/fakescreening"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/party"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, &testPool)) }

// recorder keeps the emitted events instead of publishing them.
type recorder struct{ events []outbox.Event }

func (r *recorder) Emit(_ context.Context, _ postgres.Tx, e outbox.Event) error {
	r.events = append(r.events, e)
	return nil
}

func (r *recorder) types() []string {
	types := make([]string, len(r.events))
	for i, e := range r.events {
		types[i] = e.Type
	}
	return types
}

// resolution is what a module's callback saw.
type resolution struct {
	entity, subject ids.ID
	cleared         bool
}

// world is a compliance service of an entity whose party cases are resolved into seen.
type world struct {
	entity  ids.ID
	svc     *compliance.Service
	events  *recorder
	seen    []resolution
	failing error // what the callback answers
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{entity: ids.New(), events: &recorder{}}
	w.svc = compliance.New(w.events)
	w.svc.OnResolved(compliance.CaseParty, func(_ context.Context, _ postgres.Tx, entity, subject ids.ID, cleared bool) error {
		w.seen = append(w.seen, resolution{entity, subject, cleared})
		return w.failing
	})
	return w
}

func (w *world) do(t *testing.T, fn func(tx postgres.Tx) error) error {
	t.Helper()
	return postgres.NewUnitOfWork(testPool).Do(t.Context(), fn)
}

// partyOf is a party of the world's entity whose external reference starts with prefix.
func (w *world) partyOf(prefix string) party.Party {
	return party.Party{ID: ids.New(), LegalEntityID: w.entity, Kind: party.KindNatural, ExternalRef: prefix + ids.New().String()}
}

// screen screens p in its own transaction.
func (w *world) screen(t *testing.T, p party.Party) compliance.Outcome {
	t.Helper()
	var o compliance.Outcome
	if err := w.do(t, func(tx postgres.Tx) error {
		var err error
		o, err = w.svc.ScreenParty(t.Context(), tx, p)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return o
}

// caseRow is a case as stored.
type caseRow struct {
	id                    ids.ID
	kind, status          string
	vendorRef, resolvedBy *string
	resolved              bool
}

// casesOf are the cases of subject, oldest first.
func casesOf(t *testing.T, subject ids.ID) []caseRow {
	t.Helper()
	rows, err := testPool.Query(t.Context(), `SELECT id, kind, status, vendor_ref, resolved_by, resolved_at IS NOT NULL
		FROM compliance.cases WHERE subject_id = $1 ORDER BY created_at`, subject)
	if err != nil {
		t.Fatal(err)
	}
	cases, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (caseRow, error) {
		var c caseRow
		return c, r.Scan(&c.id, &c.kind, &c.status, &c.vendorRef, &c.resolvedBy, &c.resolved)
	})
	if err != nil {
		t.Fatal(err)
	}
	return cases
}

// onlyCase is the one case of subject.
func onlyCase(t *testing.T, subject ids.ID) caseRow {
	t.Helper()
	cases := casesOf(t, subject)
	if len(cases) != 1 {
		t.Fatalf("cases of %s = %+v, want one", subject, cases)
	}
	return cases[0]
}

// pendingCase screens a party that may match and returns its case.
func (w *world) pendingCase(t *testing.T) (party.Party, caseRow) {
	t.Helper()
	p := w.partyOf(fakescreening.MatchPrefix)
	if o := w.screen(t, p); o != compliance.OutcomePending {
		t.Fatalf("a possible match = %s, want pending", o)
	}
	return p, onlyCase(t, p.ID)
}

// resolve resolves case id in its own transaction as checker, to status to.
func (w *world) resolve(t *testing.T, entity, id ids.ID, to compliance.CaseStatus) error {
	t.Helper()
	return w.do(t, func(tx postgres.Tx) error {
		_, err := w.svc.Resolve(t.Context(), tx, entity, id, to, "checker")
		return err
	})
}

// vendor is the stored vendor reference of c, or "" if it has none.
func (c caseRow) vendor() string { return valueOf(c.vendorRef) }

// who is who resolved c, or "" if nobody did.
func (c caseRow) who() string { return valueOf(c.resolvedBy) }

func valueOf(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// TestEveryScreeningIsACase (FR-42): a party the vendor clears is OutcomeClear and leaves a cleared case
// that nobody resolved; one it may match is OutcomePending with a pending case; both keep the vendor's
// reference, and each emits its event.
func TestEveryScreeningIsACase(t *testing.T) {
	w := newWorld(t)
	clean, matched := w.partyOf("onboarding-"), w.partyOf(fakescreening.MatchPrefix)

	cleanOutcome, matchOutcome := w.screen(t, clean), w.screen(t, matched)

	cleared, pending := onlyCase(t, clean.ID), onlyCase(t, matched.ID)
	if cleanOutcome != compliance.OutcomeClear || cleared.kind != "party" || cleared.status != "cleared" ||
		cleared.vendor() != "fake-"+clean.ID.String() || cleared.resolvedBy != nil || cleared.resolved {
		t.Fatalf("a clear party = %s with case %+v, want clear, cleared by the vendor", cleanOutcome, cleared)
	}
	if matchOutcome != compliance.OutcomePending || pending.status != "pending" || pending.vendor() != "fake-"+matched.ID.String() {
		t.Fatalf("a possible match = %s with case %+v, want pending with the vendor's reference", matchOutcome, pending)
	}
	expectEvents(t, w, pending.id)
}

// expectEvents checks that w emitted cleared and then pending, about case pending of its entity.
func expectEvents(t *testing.T, w *world, pending ids.ID) {
	t.Helper()
	want := []string{"aluna.compliance.screening.cleared.v1", "aluna.compliance.screening.pending.v1"}
	if got := w.events.types(); !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if e := w.events.events[1]; e.Subject != "screening-cases/"+pending.String() || e.LegalEntityID != w.entity {
		t.Fatalf("the pending event = %+v, want it about case %s of the entity", e, pending)
	}
}

// TestAVendorThatDoesNotAnswerLeavesACaseWithoutReference (FR-42, AD-3): a party the vendor does not answer
// for is OutcomePending, with a pending case and no vendor reference.
func TestAVendorThatDoesNotAnswerLeavesACaseWithoutReference(t *testing.T) {
	w := newWorld(t)
	p := w.partyOf(fakescreening.SlowPrefix)

	o := w.screen(t, p)

	if c := onlyCase(t, p.ID); o != compliance.OutcomePending || c.status != "pending" || c.vendorRef != nil {
		t.Fatalf("a vendor that does not answer = %s with case %+v, want pending without reference", o, c)
	}
}

// TestAPaymentIsScreenedLikeAParty (FR-42): an outgoing payment's beneficiary leaves a payment case about
// the order.
func TestAPaymentIsScreenedLikeAParty(t *testing.T) {
	w := newWorld(t)
	order := ids.New()
	beneficiary := compliance.Subject{Kind: compliance.SubjectBeneficiary, ID: ids.New(), ExternalRef: fakescreening.MatchPrefix + "acme"}

	var o compliance.Outcome
	err := w.do(t, func(tx postgres.Tx) error {
		var err error
		o, err = w.svc.ScreenPayment(t.Context(), tx, w.entity, order, beneficiary)
		return err
	})

	if c := onlyCase(t, order); err != nil || o != compliance.OutcomePending || c.kind != "payment" || c.status != "pending" {
		t.Fatalf("a payment to a possible match = %s (%v) with case %+v, want a pending payment case", o, err, c)
	}
}

// TestResolvingACaseTellsItsKind (FR-42): resolving a pending case records who resolved it and when, emits
// cleared or confirmed, and calls the callback of its kind with the entity, the subject and the decision.
func TestResolvingACaseTellsItsKind(t *testing.T) {
	w := newWorld(t)
	first, firstCase := w.pendingCase(t)
	second, secondCase := w.pendingCase(t)
	before := len(w.events.events)

	err := errors.Join(w.resolve(t, w.entity, firstCase.id, compliance.CaseCleared), w.resolve(t, w.entity, secondCase.id, compliance.CaseConfirmed))
	if err != nil {
		t.Fatal(err)
	}

	cleared, confirmed := onlyCase(t, first.ID), onlyCase(t, second.ID)
	if cleared.status != "cleared" || confirmed.status != "confirmed" || cleared.who() != "checker" || !cleared.resolved ||
		confirmed.who() != "checker" {
		t.Fatalf("resolved cases = %+v and %+v, want cleared and confirmed by checker", cleared, confirmed)
	}
	want := []resolution{{w.entity, first.ID, true}, {w.entity, second.ID, false}}
	if !slices.Equal(w.seen, want) {
		t.Fatalf("callbacks = %+v, want %+v", w.seen, want)
	}
	if got := w.events.types()[before:]; !slices.Equal(got, []string{"aluna.compliance.screening.cleared.v1", "aluna.compliance.screening.confirmed.v1"}) {
		t.Fatalf("resolution events = %v, want cleared and confirmed", got)
	}
}

// TestACaseIsResolvedOnce (FR-42): a resolved case, or one the vendor cleared, is ErrCaseResolved, to check
// it before requesting a resolution or to resolve it; another entity's case is ErrCaseNotFound.
func TestACaseIsResolvedOnce(t *testing.T) {
	w := newWorld(t)
	_, pending := w.pendingCase(t)
	clean := w.partyOf("onboarding-")
	w.screen(t, clean)
	if err := w.resolve(t, w.entity, pending.id, compliance.CaseCleared); err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct {
		entity, id ids.ID
		want       error
	}{
		"resolved":              {w.entity, pending.id, compliance.ErrCaseResolved},
		"cleared by the vendor": {w.entity, onlyCase(t, clean.ID).id, compliance.ErrCaseResolved},
		"of another entity":     {ids.New(), pending.id, compliance.ErrCaseNotFound},
		"of nowhere":            {w.entity, ids.New(), compliance.ErrCaseNotFound},
	}
	for name, tc := range cases {
		checked := w.svc.CheckResolution(t.Context(), testPool, tc.entity, tc.id)
		if resolved := w.resolve(t, tc.entity, tc.id, compliance.CaseConfirmed); !errors.Is(checked, tc.want) || !errors.Is(resolved, tc.want) {
			t.Errorf("%s: check = %v and resolve = %v, want %v", name, checked, resolved, tc.want)
		}
	}
	if len(w.seen) != 1 {
		t.Fatalf("callbacks = %+v, want only the first resolution's", w.seen)
	}
}

// TestACallbackThatFailsResolvesNothing (FR-42): if the callback of the case's kind fails, the resolution
// fails and the case stays pending.
func TestACallbackThatFailsResolvesNothing(t *testing.T) {
	w := newWorld(t)
	_, pending := w.pendingCase(t)
	w.failing = errors.New("the party moved on")

	err := w.resolve(t, w.entity, pending.id, compliance.CaseCleared)

	if checked := w.svc.CheckResolution(t.Context(), testPool, w.entity, pending.id); !errors.Is(err, w.failing) || checked != nil {
		t.Fatalf("a failing callback = %v, and then the case = %v; want its error and the case still pending", err, checked)
	}
}

// TestAKindWithoutCallbackIsNotResolved: a case of a kind no module registered is not resolved.
func TestAKindWithoutCallbackIsNotResolved(t *testing.T) {
	w := newWorld(t)
	order := ids.New()
	err := w.do(t, func(tx postgres.Tx) error {
		_, err := w.svc.ScreenPayment(t.Context(), tx, w.entity, order, compliance.Subject{
			Kind: compliance.SubjectBeneficiary, ID: ids.New(), ExternalRef: fakescreening.MatchPrefix + "acme",
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := w.resolve(t, w.entity, onlyCase(t, order).id, compliance.CaseCleared); err == nil || onlyCase(t, order).status != "pending" {
		t.Fatalf("resolving a payment case without callback = %v, want an error and the case pending", err)
	}
}

// TestTwoResolutionsOfACaseResolveItOnce (FR-42): a resolution waits for another in flight on the same case
// and then finds it resolved.
func TestTwoResolutionsOfACaseResolveItOnce(t *testing.T) {
	w := newWorld(t)
	p, pending := w.pendingCase(t)
	second := make(chan error, 1)

	err := w.do(t, func(tx postgres.Tx) error {
		if _, err := w.svc.Resolve(t.Context(), tx, w.entity, pending.id, compliance.CaseCleared, "first"); err != nil {
			return err
		}
		go func() { second <- w.resolve(t, w.entity, pending.id, compliance.CaseConfirmed) }()
		select {
		case err := <-second:
			return errors.Join(errors.New("the second resolution did not wait for the first"), err)
		case <-time.After(300 * time.Millisecond):
			return nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := <-second; !errors.Is(err, compliance.ErrCaseResolved) || onlyCase(t, p.ID).who() != "first" {
		t.Fatalf("the second resolution = %v, want ErrCaseResolved after the first", err)
	}
}

// asOwner runs sql as the tables' owner and returns its error.
func asOwner(t *testing.T, sql string, args ...any) error {
	t.Helper()
	return pgx.BeginFunc(t.Context(), testPool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), "SET LOCAL ROLE NONE"); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), sql, args...)
		return err
	})
}

// resolvedAs is an UPDATE that resolves a case as cleared, by x, and also sets what more says.
func resolvedAs(more string) string {
	return `UPDATE compliance.cases SET status = 'cleared', resolved_by = 'x'` + more + ` WHERE id = $1`
}

// TestTheDatabaseKeepsTheCases (FR-42): not even the owner changes a case other than resolving a pending
// one once, with who and when, nor deletes or truncates them.
func TestTheDatabaseKeepsTheCases(t *testing.T) {
	w := newWorld(t)
	_, pending := w.pendingCase(t)
	resolved := w.partyOf("onboarding-")
	w.screen(t, resolved)
	cleared := onlyCase(t, resolved.ID).id

	for name, sql := range map[string]string{
		"change the subject":                `UPDATE compliance.cases SET subject_id = gen_random_uuid() WHERE id = $1`,
		"change the vendor's ref":           `UPDATE compliance.cases SET vendor_ref = 'other' WHERE id = $1`,
		"resolve without who":               `UPDATE compliance.cases SET status = 'cleared' WHERE id = $1`,
		"resolve and change the subject":    resolvedAs(`, subject_id = gen_random_uuid()`),
		"resolve and change the vendor ref": resolvedAs(`, vendor_ref = 'other'`),
		"resolve and change the kind":       resolvedAs(`, kind = 'payment'`),
		"resolve and change the date":       resolvedAs(`, created_at = now() - interval '1 day'`),
		"resolve and stay pending":          `UPDATE compliance.cases SET resolved_by = 'x', resolved_at = now() WHERE id = $1`,
	} {
		if err := asOwner(t, sql, pending.id); err == nil {
			t.Errorf("%s: the database let it through", name)
		}
	}
	if err := asOwner(t, `UPDATE compliance.cases SET status = 'pending', resolved_by = NULL, resolved_at = NULL WHERE id = $1`, cleared); err == nil {
		t.Error("back to pending: the database let it through")
	}
	for name, tc := range map[string]struct {
		sql  string
		args []any
	}{
		"delete":   {`DELETE FROM compliance.cases WHERE id = $1`, []any{pending.id}},
		"truncate": {`TRUNCATE compliance.cases`, nil},
	} {
		if err := asOwner(t, tc.sql, tc.args...); err == nil || !strings.Contains(err.Error(), "never deleted or truncated") {
			t.Errorf("%s = %v, want the guard's refusal", name, err)
		}
	}

	if err := asOwner(t, `UPDATE compliance.cases SET status = 'confirmed', resolved_by = 'x', resolved_at = now() WHERE id = $1`, cleared); err == nil {
		t.Error("resolve a cleared case: the database let it through")
	}
}

// TestTheDatabaseStampsTheResolution (FR-42): a case is resolved when the database says, not when the caller
// does: a resolution backdated a year, or without a time, gets the database's.
func TestTheDatabaseStampsTheResolution(t *testing.T) {
	w := newWorld(t)
	for _, when := range []string{`, resolved_at = now() - interval '1 year'`, `, resolved_at = NULL`} {
		_, pending := w.pendingCase(t)
		if err := asOwner(t, resolvedAs(when), pending.id); err != nil {
			t.Fatalf("resolving a pending case with who = %v, want it accepted", err)
		}
		var stamped bool
		err := testPool.QueryRow(t.Context(), `SELECT resolved_at >= created_at FROM compliance.cases WHERE id = $1`, pending.id).Scan(&stamped)
		if err != nil || !stamped {
			t.Fatalf("a resolution with %q = stamped %v (%v), want the database's time", when, stamped, err)
		}
	}
}
