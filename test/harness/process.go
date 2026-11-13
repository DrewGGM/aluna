//go:build integration || e2e || load || restore

// Package harness runs the compiled aluna binary for the tests that drive it as a process: it builds it, starts its
// commands with their logs captured, waits for the api and signs its tokens with a test OIDC issuer. The build tags
// keep it out of every production build.
package harness

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// Build compiles cmd/aluna with the go build flags into a temporary directory of t and returns the binary's path.
func Build(t testing.TB, flags ...string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "aluna")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	args := append(append([]string{"build"}, flags...), "-o", bin, "github.com/DrewGGM/aluna/cmd/aluna")
	if out, err := exec.CommandContext(t.Context(), "go", args...).CombinedOutput(); err != nil { //nolint:gosec // the flags are the test's own and bin is a path in its TempDir
		t.Fatalf("build aluna: %v\n%s", err, out)
	}
	return bin
}

// Command prepares bin with args and the process environment plus env, without exporting telemetry. It dies with the
// test's context.
func Command(t testing.TB, bin string, env []string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), bin, args...) //nolint:gosec // bin was built by Build
	cmd.Env = append(os.Environ(), append([]string{"OTEL_EXPORTER_OTLP_ENDPOINT="}, env...)...)
	return cmd
}

// Migrate runs aluna migrate up on dsn, which connects as the tables' owner.
func Migrate(t testing.TB, bin, dsn string) {
	t.Helper()
	if out, err := Command(t, bin, []string{"ALUNA_DATABASE_URL=" + dsn}, "migrate", "up").CombinedOutput(); err != nil {
		t.Fatalf("aluna migrate up: %v\n%s", err, out)
	}
}

// Start runs aluna command with env and returns what kills it and waits for it to exit. Otherwise it dies when the
// test ends; if the test failed, its log is printed.
func Start(t testing.TB, bin, command string, env ...string) (stop func()) {
	t.Helper()
	cmd := Command(t, bin, env, command)
	var logs bytes.Buffer // os/exec does not write to Stdout and Stderr concurrently if they are the same writer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	// a child that inherited the log pipe would otherwise keep Wait from returning after the kill
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	wait := sync.OnceValue(cmd.Wait)
	t.Cleanup(func() {
		wait() //nolint:errcheck,gosec // the process dies with the test context
		if t.Failed() {
			t.Logf("aluna %s log:\n%s", command, logs.String())
		}
	})
	return func() {
		err := cmd.Process.Kill()
		if errors.Is(err, os.ErrProcessDone) {
			t.Errorf("aluna %s exited before it was stopped: %v", command, wait())
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		wait() //nolint:errcheck,gosec // killed on purpose
	}
}

// StartAPI starts aluna api on appDSN, trusting issuer's tokens and taking the simulated rail's notifications signed
// with railSecret on railAddr, and returns its base URL once /readyz answers 200.
func StartAPI(t testing.TB, bin, appDSN, railAddr, railSecret string, issuer *Issuer) (base string) {
	t.Helper()
	addr := FreeAddr(t)
	Start(t, bin, "api", "ALUNA_HTTP_ADDR="+addr, "ALUNA_RAIL_ADDR="+railAddr, "ALUNA_DATABASE_URL="+appDSN,
		"ALUNA_OIDC_ISSUER="+issuer.URL, "ALUNA_OIDC_AUDIENCE="+audience, "ALUNA_SIMBREB_SECRET="+railSecret)
	base = "http://" + addr
	waitReady(t, base+"/readyz")
	return base
}

// FreeAddr reserves a free loopback port and releases it for the process to take.
func FreeAddr(t testing.TB) string {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// readyClient keeps a hung process from hanging waitReady.
var readyClient = &http.Client{Timeout: 10 * time.Second}

// waitReady polls url until it answers 200, for up to 30 s; while the process starts, the connection fails.
func waitReady(t testing.TB, url string) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp, err := readyClient.Do(req); err == nil {
			resp.Body.Close() //nolint:errcheck,gosec // only the status is read
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
	}
	t.Fatalf("%s did not answer 200 within 30 s", url)
}
