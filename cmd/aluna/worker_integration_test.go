//go:build integration

package main

import (
	"io"
	"os"
	"runtime"
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
