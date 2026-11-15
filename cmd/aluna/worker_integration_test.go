//go:build integration

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// TestRunWorkerStopsOnSIGTERM: aluna worker connects as aluna_app, wins the leader election and, on
// SIGTERM, shuts down in order and releases leadership so another worker can take it.
func TestRunWorkerStopsOnSIGTERM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot send SIGTERM to a process; runs in CI (Linux)")
	}
	appDSN, err := pgtest.AppDSN(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ALUNA_DATABASE_URL", appDSN)
	t.Setenv("ALUNA_AUDIT_ANCHOR_DIR", t.TempDir())
	t.Setenv("ALUNA_SIMBREB_SECRET", railSecret)
	exit := make(chan int, 1)
	go func() { exit <- run([]string{"worker"}, io.Discard) }()
	waitLeaders(t, 1)

	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := self.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	expectExit(t, exit, 0)
	waitLeaders(t, 0)
}

// TestTheWorkerDoesNotStartOnAnAnchorAhead (Q-138 f): an anchor that holds the block after the database's last one,
// as after a restore it did not follow, stops the worker before it takes a job, and says why.
func TestTheWorkerDoesNotStartOnAnAnchorAhead(t *testing.T) {
	appDSN, err := pgtest.AppDSN(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	var last int64
	pgtest.AsOwner(t, appPool(t), func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `SELECT coalesce(max(id), 0) FROM audit.blocks`).Scan(&last)
	})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%020d.sha256", last+1)), []byte(strings.Repeat("0", 64)), 0o400); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ALUNA_DATABASE_URL", appDSN)
	t.Setenv("ALUNA_AUDIT_ANCHOR_DIR", dir)
	t.Setenv("ALUNA_SIMBREB_SECRET", railSecret)

	var logs bytes.Buffer // read after the worker returned
	exit := make(chan int, 1)
	go func() { exit <- run([]string{"worker"}, &logs) }()
	expectExit(t, exit, 1)

	if want := fmt.Sprintf("the audit anchor has block %d", last+1); !strings.Contains(logs.String(), want) {
		t.Fatalf("logs %q, want %q", logs.String(), want)
	}
}

// waitLeaders waits up to 10 s for infra.river_leader to have want rows.
func waitLeaders(t *testing.T, want int) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context()) //nolint:errcheck // the test is done with the connection
	deadline := time.Now().Add(10 * time.Second)
	for {
		var leaders int
		if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM infra.river_leader`).Scan(&leaders); err != nil {
			t.Fatal(err)
		}
		if leaders == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("infra.river_leader has %d rows, want %d", leaders, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
