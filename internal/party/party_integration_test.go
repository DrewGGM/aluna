//go:build integration

package party_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

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

func input(entity ids.ID) party.RegisterInput {
	return party.RegisterInput{
		LegalEntityID: entity, Kind: party.KindNatural, ExternalRef: "onboarding-" + ids.New().String(),
		RiskLevel: party.RiskMedium, PEP: true, Consents: map[string]bool{"personal_data": true, "marketing": false},
	}
}

func register(t *testing.T, svc *party.Service, in party.RegisterInput) (party.Party, error) {
	t.Helper()
	var p party.Party
	err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		var err error
		p, err = svc.Register(t.Context(), tx, in)
		return err
	})
	return p, err
}

// TestRegisterStoresTheReferenceAndAFirstVersion (FR-16): a new party keeps its kind, its onboarding
// reference and a version 1 in KYC pending with risk, PEP and consents; Get returns it unchanged.
func TestRegisterStoresTheReferenceAndAFirstVersion(t *testing.T) {
	svc := party.New(&recorder{})
	in := input(ids.New())

	p, err := register(t, svc, in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(t.Context(), testPool, in.LegalEntityID, p.ID)
	if err != nil {
		t.Fatal(err)
	}

	want := party.Party{
		ID: p.ID, LegalEntityID: in.LegalEntityID, Kind: party.KindNatural, ExternalRef: in.ExternalRef, CreatedAt: got.CreatedAt,
		Current: party.Version{
			Number: 1, KYCStatus: party.KYCPending, RiskLevel: party.RiskMedium, PEP: true, Consents: in.Consents,
			ValidFrom: got.Current.ValidFrom, RecordedAt: got.Current.RecordedAt,
		},
	}
	if !reflect.DeepEqual(got, want) || got.Current.ValidFrom.IsZero() || got.CreatedAt.IsZero() {
		t.Fatalf("party = %+v, want %+v", got, want)
	}
}

// TestRegisterWithoutConsents: a party registered without consents stores and returns an empty set.
func TestRegisterWithoutConsents(t *testing.T) {
	svc := party.New(&recorder{})
	in := input(ids.New())
	in.Consents = nil

	p, err := register(t, svc, in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(t.Context(), testPool, in.LegalEntityID, p.ID)

	if err != nil || got.Current.Consents == nil || len(got.Current.Consents) != 0 {
		t.Fatalf("consents = %#v, %v; want an empty set", got.Current.Consents, err)
	}
}

// TestGetReturnsTheLatestVersion: with several versions, Get returns the highest one.
func TestGetReturnsTheLatestVersion(t *testing.T) {
	svc := party.New(&recorder{})
	p, err := register(t, svc, input(ids.New()))
	if err != nil {
		t.Fatal(err)
	}
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO party.versions (party_id, version, kyc_status, risk_level, pep, consents)
			VALUES ($1, 2, 'approved', 'high', false, '{}')`, p.ID)
		return err
	})

	got, err := svc.Get(t.Context(), testPool, p.LegalEntityID, p.ID)

	if err != nil || got.Current.Number != 2 || got.Current.KYCStatus != party.KYCApproved || got.Current.RiskLevel != party.RiskHigh {
		t.Fatalf("current version = %+v, %v; want version 2", got.Current, err)
	}
}

// TestRegistrationEmitsAnEventWithoutPersonalData: registering emits aluna.party.party.registered.v1 with the
// party's state and without its external_ref, which onboarding may set to a document number.
func TestRegistrationEmitsAnEventWithoutPersonalData(t *testing.T) {
	events := &recorder{}
	in := input(ids.New())

	p, err := register(t, party.New(events), in)
	if err != nil {
		t.Fatal(err)
	}

	if len(events.events) != 1 {
		t.Fatalf("events = %+v, want one", events.events)
	}
	e := events.events[0]
	data, err := json.Marshal(e.Data)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"party_id":"` + p.ID.String() + `","kind":"natural","kyc_status":"pending","risk_level":"medium","pep":true}`
	if e.Type != "aluna.party.party.registered.v1" || e.Subject != "parties/"+p.ID.String() || e.LegalEntityID != in.LegalEntityID || string(data) != want {
		t.Fatalf("event = %+v with %s", e, data)
	}
}

// TestAnExternalRefIsRegisteredOnce: an entity does not register the same external_ref twice; another
// entity may use it.
func TestAnExternalRefIsRegisteredOnce(t *testing.T) {
	svc := party.New(&recorder{})
	in := input(ids.New())
	if _, err := register(t, svc, in); err != nil {
		t.Fatal(err)
	}

	_, again := register(t, svc, in)
	other := in
	other.LegalEntityID = ids.New()
	_, elsewhere := register(t, svc, other)

	if !errors.Is(again, party.ErrAlreadyRegistered) || elsewhere != nil {
		t.Fatalf("same external_ref = %v, in another entity = %v", again, elsewhere)
	}
}

// TestRegisterRejectsInvalidData: kind, external_ref, risk level and consents are validated before
// storing; what the database would reject (NUL, an index row too long) is a 422, not a 500.
func TestRegisterRejectsInvalidData(t *testing.T) {
	svc := party.New(&recorder{})
	tooMany := map[string]bool{}
	for i := range 33 {
		tooMany[fmt.Sprintf("consent_%d", i)] = true
	}
	for name, change := range map[string]func(*party.RegisterInput){
		"unknown kind":              func(in *party.RegisterInput) { in.Kind = "company" },
		"blank external_ref":        func(in *party.RegisterInput) { in.ExternalRef = "   " },
		"external_ref too long":     func(in *party.RegisterInput) { in.ExternalRef = strings.Repeat("x", 256) },
		"external_ref with NUL":     func(in *party.RegisterInput) { in.ExternalRef = "onb\x00ref" },
		"zero-width external_ref":   func(in *party.RegisterInput) { in.ExternalRef = "onb\u200bref" },
		"unknown risk level":        func(in *party.RegisterInput) { in.RiskLevel = "extreme" },
		"unnamed consent":           func(in *party.RegisterInput) { in.Consents = map[string]bool{" ": true} },
		"consent name with symbols": func(in *party.RegisterInput) { in.Consents = map[string]bool{"Personal-Data": true} },
		"too many consents":         func(in *party.RegisterInput) { in.Consents = tooMany },
	} {
		t.Run(name, func(t *testing.T) {
			in := input(ids.New())
			change(&in)
			if _, err := register(t, svc, in); !errors.Is(err, party.ErrInvalidParty) {
				t.Fatalf("Register = %v, want ErrInvalidParty", err)
			}
		})
	}
}

// TestAnotherEntitysPartyDoesNotExist: Get and KYCSatisfies only see the parties of the entity they are
// given; another entity's party is ErrNotFound.
func TestAnotherEntitysPartyDoesNotExist(t *testing.T) {
	svc := party.New(&recorder{})
	p, err := register(t, svc, input(ids.New()))
	if err != nil {
		t.Fatal(err)
	}
	other := ids.New()

	_, getErr := svc.Get(t.Context(), testPool, other, p.ID)
	_, kycErr := svc.KYCSatisfies(t.Context(), testPool, other, p.ID, []party.KYCStatus{party.KYCPending})

	if !errors.Is(getErr, party.ErrNotFound) || !errors.Is(kycErr, party.ErrNotFound) {
		t.Fatalf("Get = %v, KYCSatisfies = %v from another entity, want ErrNotFound", getErr, kycErr)
	}
}

// TestKYCSatisfies (FR-17): it reports whether the current KYC status is among the allowed ones; a party
// that does not exist is ErrNotFound.
func TestKYCSatisfies(t *testing.T) {
	svc := party.New(&recorder{})
	p, err := register(t, svc, input(ids.New()))
	if err != nil {
		t.Fatal(err)
	}

	entity := p.LegalEntityID
	pending, errPending := svc.KYCSatisfies(t.Context(), testPool, entity, p.ID, []party.KYCStatus{party.KYCPending, party.KYCApproved})
	approvedOnly, errApproved := svc.KYCSatisfies(t.Context(), testPool, entity, p.ID, []party.KYCStatus{party.KYCApproved})
	_, errMissing := svc.KYCSatisfies(t.Context(), testPool, entity, ids.New(), []party.KYCStatus{party.KYCApproved})

	if !pending || errPending != nil || approvedOnly || errApproved != nil {
		t.Fatalf("pending allowed = %v (%v), approved only = %v (%v)", pending, errPending, approvedOnly, errApproved)
	}
	if !errors.Is(errMissing, party.ErrNotFound) {
		t.Fatalf("missing party = %v, want ErrNotFound", errMissing)
	}
}

// TestPartiesAndVersionsAreImmutable: neither a party nor a version is updated, deleted or truncated, not
// even by the table owner: a change is a new version.
func TestPartiesAndVersionsAreImmutable(t *testing.T) {
	p, err := register(t, party.New(&recorder{}), input(ids.New()))
	if err != nil {
		t.Fatal(err)
	}
	id := p.ID.String()
	for name, sql := range map[string]string{
		"update the party":      `UPDATE party.parties SET external_ref = 'other' WHERE id = '` + id + `'`,
		"delete the party":      `DELETE FROM party.parties WHERE id = '` + id + `'`,
		"update a version":      `UPDATE party.versions SET kyc_status = 'approved' WHERE party_id = '` + id + `'`,
		"delete a version":      `DELETE FROM party.versions WHERE party_id = '` + id + `'`,
		"truncate the versions": `TRUNCATE party.versions`,
		"truncate the parties":  `TRUNCATE party.parties CASCADE`,
	} {
		if err := ownerExec(t, sql); pgCode(err) != "2F003" {
			t.Errorf("%s as owner = %v, want the database to forbid it", name, err)
		}
	}
}

// ownerExec runs sql as the table owner, inside a savepoint that is always rolled back, and returns its
// error.
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

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
