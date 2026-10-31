//go:build integration

package main

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DrewGGM/aluna/internal/audit"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// TestAuditVerifyOverTheCLI: aluna audit verify exits with 0 on an untouched chain and with 1, naming the
// block, when someone altered a sealed record.
func TestAuditVerifyOverTheCLI(t *testing.T) {
	appDSN, err := pgtest.AppDSN(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ALUNA_DATABASE_URL", appDSN)
	t.Setenv("ALUNA_AUDIT_ANCHOR_DIR", t.TempDir())
	defer func(w io.Writer) { stderr = w }(stderr)
	block, correlation := sealedRecord(t, appDSN)
	verify := []string{"audit", "verify", "--from", strconv.FormatInt(block, 10), "--to", strconv.FormatInt(block, 10)}

	var out, errOut bytes.Buffer
	stderr = &errOut
	if code := run(verify, &out); code != 0 || !strings.Contains(out.String(), "intact") {
		t.Fatalf("verify of an untouched chain = %d: %s %s", code, out.String(), errOut.String())
	}
	alterAsOwner(t, correlation)
	errOut.Reset()

	if code := run(verify, &out); code != 1 || !strings.Contains(errOut.String(), "block "+strconv.FormatInt(block, 10)) {
		t.Fatalf("verify of an altered chain = %d: %s", code, errOut.String())
	}
}

// sealedRecord stores a record and seals everything pending into a block with the anchor in
// ALUNA_AUDIT_ANCHOR_DIR; it returns the block and the record's correlation ID.
func sealedRecord(t *testing.T, dsn string) (int64, string) {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	records, err := auditFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	uow, correlation := postgres.NewUnitOfWork(pool), ids.New().String()
	err = uow.Do(t.Context(), func(tx postgres.Tx) error {
		return records.Record(t.Context(), tx, audit.Record{
			Actor: "operator", Role: "treasurer", Channel: "api", CorrelationID: correlation, Command: "POST /v1/transactions", Result: "ok",
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	block, err := records.Seal(t.Context(), uow, 10_000)
	if err != nil || block == 0 {
		t.Fatalf("seal = %d, %v", block, err)
	}
	return block, correlation
}

// alterAsOwner changes the result of the sealed record with the triggers disabled, as a database
// administrator could.
func alterAsOwner(t *testing.T, correlation string) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context()) //nolint:errcheck // the test is done with the connection
	err = pgx.BeginFunc(t.Context(), conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), "ALTER TABLE audit.records DISABLE TRIGGER USER"); err != nil {
			return err
		}
		if _, err := tx.Exec(t.Context(), "UPDATE audit.records SET result = 'ok-forged' WHERE correlation_id = $1", correlation); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), "ALTER TABLE audit.records ENABLE TRIGGER USER")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
