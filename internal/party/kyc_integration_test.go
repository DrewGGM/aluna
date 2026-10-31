//go:build integration

package party_test

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/party"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// changeKYC changes the party to status to, requested against its current version.
func changeKYC(t *testing.T, svc *party.Service, p party.Party, to party.KYCStatus) (party.Party, error) {
	t.Helper()
	current, err := svc.Get(t.Context(), testPool, p.LegalEntityID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	return changeKYCAt(t, svc, p, current.Current.Number, to)
}

// changeKYCAt changes the party to status to, requested against version seen.
func changeKYCAt(t *testing.T, svc *party.Service, p party.Party, seen int, to party.KYCStatus) (party.Party, error) {
	t.Helper()
	var changed party.Party
	err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		var err error
		changed, err = svc.ChangeKYC(t.Context(), tx, p.LegalEntityID, p.ID, seen, to)
		return err
	})
	return changed, err
}

// TestChangeKYCRecordsANewVersion (FR-16): a valid change records version 2 with the new KYC status and the
// rest of the state unchanged; Get returns it as current.
func TestChangeKYCRecordsANewVersion(t *testing.T) {
	svc := party.New(&recorder{})
	p, err := register(t, svc, input(ids.New()))
	if err != nil {
		t.Fatal(err)
	}

	changed, err := changeKYC(t, svc, p, party.KYCApproved)
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(t.Context(), testPool, p.LegalEntityID, p.ID)
	if err != nil {
		t.Fatal(err)
	}

	want := p.Current
	want.Number, want.KYCStatus = 2, party.KYCApproved
	want.ValidFrom, want.RecordedAt = got.Current.ValidFrom, got.Current.RecordedAt
	if !reflect.DeepEqual(got.Current, want) || !reflect.DeepEqual(changed.Current, got.Current) || !want.ValidFrom.After(p.Current.ValidFrom) {
		t.Fatalf("current version = %+v, want %+v: version 2 approved with the rest unchanged", got.Current, want)
	}
}

// TestKYCChangesFollowTheTransitions: pending → approved → suspended → approved is valid; from rejected, or
// back to pending, it is ErrInvalidKYCTransition and no version is recorded.
func TestKYCChangesFollowTheTransitions(t *testing.T) {
	svc := party.New(&recorder{})
	p, err := register(t, svc, input(ids.New()))
	if err != nil {
		t.Fatal(err)
	}
	for _, to := range []party.KYCStatus{party.KYCApproved, party.KYCSuspended, party.KYCApproved} {
		if _, err := changeKYC(t, svc, p, to); err != nil {
			t.Fatalf("change to %s = %v", to, err)
		}
	}
	if _, err := changeKYC(t, svc, p, party.KYCPending); !errors.Is(err, party.ErrInvalidKYCTransition) {
		t.Fatalf("approved to pending = %v, want ErrInvalidKYCTransition", err)
	}

	rejected, err := register(t, svc, input(ids.New()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := changeKYC(t, svc, rejected, party.KYCRejected); err != nil {
		t.Fatal(err)
	}
	_, again := changeKYC(t, svc, rejected, party.KYCApproved)
	got, err := svc.Get(t.Context(), testPool, rejected.LegalEntityID, rejected.ID)

	if !errors.Is(again, party.ErrInvalidKYCTransition) || err != nil || got.Current.Number != 2 {
		t.Fatalf("rejected to approved = %v, current version %d (%v); want ErrInvalidKYCTransition at version 2", again, got.Current.Number, err)
	}
}

// TestConcurrentKYCChangesAreSerialized: while a change of a party is uncommitted, a second one requested
// against the same version waits for it and is then stale, instead of also writing version 2.
func TestConcurrentKYCChangesAreSerialized(t *testing.T) {
	svc := party.New(&recorder{})
	p, err := register(t, svc, input(ids.New()))
	if err != nil {
		t.Fatal(err)
	}
	changed, release, first := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce() // if the test fails early, the first transaction is not left open
	go func() {
		first <- postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
			if _, err := svc.ChangeKYC(t.Context(), tx, p.LegalEntityID, p.ID, 1, party.KYCApproved); err != nil {
				return err
			}
			close(changed)
			<-release
			return nil
		})
	}()
	select {
	case <-changed:
	case err := <-first:
		t.Fatalf("the first change did not happen: %v", err)
	}
	second := make(chan error, 1)
	go func() {
		_, err := changeKYCAt(t, svc, p, 1, party.KYCRejected)
		second <- err
	}()
	waitForAdvisoryLockWait(t)
	releaseOnce()

	if err := <-first; err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(t.Context(), testPool, p.LegalEntityID, p.ID)
	if secondErr := <-second; !errors.Is(secondErr, party.ErrStaleKYCChange) || err != nil || got.Current.Number != 2 {
		t.Fatalf("second change = %v, current version %d (%v); want ErrStaleKYCChange at version 2", secondErr, got.Current.Number, err)
	}
}

// waitForAdvisoryLockWait waits, for less than lock_timeout (2 s), until a session of this test database is
// waiting for an advisory lock: the second change is blocked behind the first. Without the lock the second
// change waits on a row instead, and this fails.
func waitForAdvisoryLockWait(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(1500 * time.Millisecond); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		var waiting int
		err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM pg_locks
			WHERE locktype = 'advisory' AND NOT granted
				AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
	}
	t.Fatal("the second change never waited for the first on the party lock")
}

// TestAStaleKYCChangeIsRejected: a change requested at one version is stale once the party moved on, even
// if its status came back to the same value (suspended, approved, suspended again).
func TestAStaleKYCChangeIsRejected(t *testing.T) {
	svc := party.New(&recorder{})
	p, err := register(t, svc, input(ids.New()))
	if err != nil {
		t.Fatal(err)
	}
	for _, to := range []party.KYCStatus{party.KYCApproved, party.KYCSuspended} {
		if _, err := changeKYC(t, svc, p, to); err != nil {
			t.Fatal(err)
		}
	}
	const requestedAt = 3 // suspended: the old request asks to approve it
	for _, to := range []party.KYCStatus{party.KYCApproved, party.KYCSuspended} {
		if _, err := changeKYC(t, svc, p, to); err != nil {
			t.Fatal(err)
		}
	}

	_, err = changeKYCAt(t, svc, p, requestedAt, party.KYCApproved)
	got, getErr := svc.Get(t.Context(), testPool, p.LegalEntityID, p.ID)

	if !errors.Is(err, party.ErrStaleKYCChange) || getErr != nil || got.Current.KYCStatus != party.KYCSuspended || got.Current.Number != 5 {
		t.Fatalf("old request = %v; party %+v (%v), want ErrStaleKYCChange and the party still suspended at version 5", err, got.Current, getErr)
	}
}

// TestCheckKYCChange: a change is checked against the current status before it waits for approval, and
// returns the version it was checked against; another entity's party does not exist.
func TestCheckKYCChange(t *testing.T) {
	svc := party.New(&recorder{})
	p, err := register(t, svc, input(ids.New()))
	if err != nil {
		t.Fatal(err)
	}

	version, valid := svc.CheckKYCChange(t.Context(), testPool, p.LegalEntityID, p.ID, party.KYCApproved)
	_, invalid := svc.CheckKYCChange(t.Context(), testPool, p.LegalEntityID, p.ID, party.KYCSuspended)
	_, foreign := svc.CheckKYCChange(t.Context(), testPool, ids.New(), p.ID, party.KYCApproved)

	if valid != nil || version != 1 || !errors.Is(invalid, party.ErrInvalidKYCTransition) || !errors.Is(foreign, party.ErrNotFound) {
		t.Fatalf("valid = %v at version %d, pending to suspended = %v, another entity = %v", valid, version, invalid, foreign)
	}
	if _, err := changeKYC(t, svc, p, party.KYCApproved); err != nil {
		t.Fatal(err)
	}
	if version, err := svc.CheckKYCChange(t.Context(), testPool, p.LegalEntityID, p.ID, party.KYCSuspended); err != nil || version != 2 {
		t.Fatalf("approved to suspended = %v at version %d, want it checked against version 2", err, version)
	}
}
