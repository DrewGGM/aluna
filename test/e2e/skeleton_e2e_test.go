//go:build e2e

// Package e2e tests the aluna binary end to end: real process, real PostgreSQL and HTTP.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/coreos/go-oidc/v3/oidc/oidctest"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// TestSkeleton: a balanced transaction of 100,000 COP travels HTTP → ledger → PostgreSQL → HTTP with the
// compiled binary, migrated by itself and connected with the application role.
func TestSkeleton(t *testing.T) {
	api := newAPI(t)

	cash, deposit := createAccount(t, api, "asset"), createAccount(t, api, "liability")
	postTransaction(t, api, cash, deposit, 10_000_000)

	for account, posted := range map[string]int64{cash: 10_000_000, deposit: -10_000_000} {
		var b struct{ Posted, Natural struct{ Amount int64 } }
		if status := call(t, api, http.MethodGet, "/v1/ledger-accounts/"+account+"/balance", "", &b); status != http.StatusOK {
			t.Fatalf("balance of %s = %d", account, status)
		}
		if b.Posted.Amount != posted || b.Natural.Amount != 10_000_000 {
			t.Errorf("balance of %s = %+v, want signed %d and natural 100,000 COP", account, b, posted)
		}
	}
}

// newAPI builds aluna, migrates a fresh database with it and starts aluna api on top of it.
func newAPI(t *testing.T) runningAPI {
	t.Helper()
	bin := buildAluna(t)
	dsn, cleanup, err := pgtest.StartEmpty(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	if out, err := alunaCmd(t, bin, []string{"ALUNA_DATABASE_URL=" + dsn}, "migrate", "up").CombinedOutput(); err != nil {
		t.Fatalf("aluna migrate up: %v\n%s", err, out)
	}
	return startAPI(t, bin, dsn)
}

// postTransaction posts units from the debit account against the credit account and returns the
// transaction ID.
func postTransaction(t *testing.T, a runningAPI, debit, credit string, units int64) string {
	t.Helper()
	posting := fmt.Sprintf(`{"code":"deposit","value_date":"2026-10-26","business_date":"2026-10-26",
		"source":"e2e","external_ref":%q,"entries":[
		{"account_id":%q,"amount":{"amount":%d,"currency":"COP"}},
		{"account_id":%q,"amount":{"amount":%d,"currency":"COP"}}]}`, ids.New().String(), debit, units, credit, -units)
	var posted struct{ ID string }
	if status := call(t, a, http.MethodPost, "/v1/transactions", posting, &posted); status != http.StatusCreated {
		t.Fatalf("post = %d", status)
	}
	return posted.ID
}

// buildAluna builds cmd/aluna into a temporary directory.
func buildAluna(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "aluna")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.CommandContext(t.Context(), "go", "build", "-race", "-o", bin, "github.com/DrewGGM/aluna/cmd/aluna").CombinedOutput(); err != nil { //nolint:gosec // bin is a path in the test's TempDir
		t.Fatalf("build aluna: %v\n%s", err, out)
	}
	return bin
}

// alunaCmd prepares the binary with the process environment plus env, without exporting telemetry.
func alunaCmd(t *testing.T, bin string, env []string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), bin, args...) //nolint:gosec // bin was built by buildAluna
	cmd.Env = append(os.Environ(), append([]string{"OTEL_EXPORTER_OTLP_ENDPOINT="}, env...)...)
	return cmd
}

// runningAPI is a running aluna api, the token used to talk to it and the issuer that signs tokens for
// other operators of the same entity.
type runningAPI struct {
	base, token string
	sign        func(subject string) string
}

// as is the same api with the session of operator subject.
func (a runningAPI) as(subject string) runningAPI {
	return runningAPI{base: a.base, token: a.sign(subject), sign: a.sign}
}

// startAPI starts aluna api with the aluna_app role and a test OIDC issuer, waits for /readyz and returns
// its URL with an operator token for a new entity. The process dies when the test ends; if the test
// fails, its log is printed in the test output.
func startAPI(t *testing.T, bin, dsn string) runningAPI {
	t.Helper()
	appDSN, err := pgtest.AppDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	addr := freeAddr(t)
	issuer, sign := oidcIssuer(t)
	cmd := alunaCmd(t, bin, []string{
		"ALUNA_HTTP_ADDR=" + addr, "ALUNA_DATABASE_URL=" + appDSN, "ALUNA_OIDC_ISSUER=" + issuer, "ALUNA_OIDC_AUDIENCE=" + audience,
	}, "api")
	var logs bytes.Buffer // os/exec does not write to Stdout and Stderr concurrently if they are the same writer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Wait() //nolint:errcheck,gosec // the process dies with the test context
		if t.Failed() {
			t.Logf("aluna api log:\n%s", logs.String())
		}
	})
	base := "http://" + addr
	waitReady(t, base+"/readyz")
	return runningAPI{base: base, token: sign("operator-e2e"), sign: sign}
}

const audience = "aluna-api"

// oidcIssuer is a test OIDC issuer and a function that signs, with the 15-minute maximum lifetime, the
// token of operator subject; all operators share a new entity.
func oidcIssuer(t *testing.T) (issuer string, sign func(subject string) string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := &oidctest.Server{PublicKeys: []oidctest.PublicKey{{PublicKey: key.Public(), KeyID: "k1", Algorithm: oidc.RS256}}}
	ts := serveIssuer(t, s)
	entity := ids.New().String()
	return ts.URL, func(subject string) string {
		now := time.Now()
		claims, err := json.Marshal(map[string]any{
			"iss": ts.URL, "aud": audience, "sub": subject, "iat": now.Unix(), "exp": now.Add(15 * time.Minute).Unix(),
			"legal_entity_id": entity, "roles": []string{"operator"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return oidctest.SignIDToken(key, "k1", oidc.RS256, string(claims))
	}
}

// serveIssuer serves s with its URL as the issuer. The URL is set before the server starts: afterwards,
// requests would read it from another goroutine while it is being written.
func serveIssuer(t *testing.T, s *oidctest.Server) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(s)
	s.SetIssuer("http://" + ts.Listener.Addr().String())
	ts.Start()
	t.Cleanup(ts.Close)
	return ts
}

// waitReady polls url until it answers 200; while the process starts, the connection fails.
func waitReady(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close() //nolint:errcheck,gosec // close in a test
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s did not answer 200 within 30 s", url)
}

func createAccount(t *testing.T, a runningAPI, kind string) string {
	t.Helper()
	var account struct{ ID string }
	body := fmt.Sprintf(`{"code":"e2e-%s","kind":%q,"currency":"COP","balance_mode":"sync"}`, ids.New(), kind)
	if status := call(t, a, http.MethodPost, "/v1/ledger-accounts", body, &account); status != http.StatusCreated {
		t.Fatalf("create %s account = %d", kind, status)
	}
	return account.ID
}

// client keeps a hung process from hanging the test.
var client = &http.Client{Timeout: 10 * time.Second}

// call makes a JSON request to path with a's token and decodes the response into out if it is not nil.
func call(t *testing.T, a runningAPI, method, path, body string, out any) int {
	t.Helper()
	url := a.base + path
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", ids.New().String())
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close() //nolint:errcheck // close in a test
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s = %d: invalid body: %v", method, url, resp.StatusCode, err)
		}
	}
	return resp.StatusCode
}

// freeAddr reserves a free port and releases it for the process to take.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}
