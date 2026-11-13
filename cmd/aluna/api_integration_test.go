//go:build integration

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"syscall"
	"testing"

	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
	"github.com/DrewGGM/aluna/test/harness"
)

var testDSN string

func TestMain(m *testing.M) {
	dsn, cleanup, err := pgtest.Start(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testDSN = dsn
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// TestRunAPIStopsOnSIGTERM tests the real path: the configuration comes from the environment, the API
// becomes ready against the database, and SIGTERM, which the orchestrator sends on a deploy, shuts it
// down in order.
func TestRunAPIStopsOnSIGTERM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot send SIGTERM to a process; runs in CI (Linux)")
	}
	addr := harness.FreeAddr(t)
	t.Setenv("ALUNA_HTTP_ADDR", addr)
	appDSN, err := pgtest.AppDSN(testDSN) // as in production: the application role, not the owner
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ALUNA_DATABASE_URL", appDSN)
	t.Setenv("ALUNA_OIDC_ISSUER", harness.NewIssuer(t).URL)
	t.Setenv("ALUNA_OIDC_AUDIENCE", "aluna-api")
	t.Setenv("ALUNA_RAIL_ADDR", harness.FreeAddr(t))
	t.Setenv("ALUNA_SIMBREB_SECRET", railSecret)
	exit := make(chan int, 1)
	go func() { exit <- run([]string{"api"}, io.Discard) }()
	waitHealthy(t, addr)
	resp, err := http.Get("http://" + addr + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("/readyz = %d (%v): with the database up the api is ready", resp.StatusCode, err)
	}

	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := self.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	expectExit(t, exit, 0)
}
