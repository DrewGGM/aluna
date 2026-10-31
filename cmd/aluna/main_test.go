package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/httpapi"
)

func TestRunVersion(t *testing.T) {
	var out bytes.Buffer

	code := run([]string{"version"}, &out)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got, want := out.String(), "aluna dev\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestRunVersionInjected(t *testing.T) {
	defer func(v string) { version = v }(version)
	version = "1.2.3"
	var out bytes.Buffer

	run([]string{"version"}, &out)

	if got, want := out.String(), "aluna 1.2.3\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestRunWithoutValidCommandPrintsUsage(t *testing.T) {
	cases := map[string][]string{
		"no arguments":                 nil,
		"unknown command":              {"frobnicate"},
		"extra arguments":              {"version", "extra"},
		"migrate without direction":    {"migrate"},
		"migrate invalid direction":    {"migrate", "sideways"},
		"migrate with extra arguments": {"migrate", "up", "extra"},
		"api with arguments":           {"api", "extra"},
		"worker with arguments":        {"worker", "extra"},
		"audit without subcommand":     {"audit"},
		"unknown audit subcommand":     {"audit", "check"},
		"verify without blocks":        {"audit", "verify"},
		"verify reversed range":        {"audit", "verify", "--from", "3", "--to", "2"},
		"verify with extra arguments":  {"audit", "verify", "--from", "1", "--to", "2", "extra"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			defer func(w io.Writer) { stderr = w }(stderr)
			var out, errOut bytes.Buffer
			stderr = &errOut

			code := run(args, &out)

			if code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if out.Len() != 0 {
				t.Errorf("stdout = %q, want empty", out.String())
			}
			if !strings.HasPrefix(errOut.String(), "usage: aluna") {
				t.Errorf("stderr = %q, want the usage text", errOut.String())
			}
		})
	}
}

func TestServeAPIShutsDownWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	exit := make(chan int, 1)
	go func() {
		exit <- serveAPI(ctx, ln, httpapi.NewServer(httpapi.ServerConfig{}, nil, quietLogger()), quietLogger())
	}()

	waitHealthy(t, ln.Addr().String())
	cancel()

	expectExit(t, exit, 0)
}

func TestRunAPIInvalidAddr(t *testing.T) {
	var out bytes.Buffer

	code := runAPI(context.Background(), apiConfig{addr: "127.0.0.1:-1"}, nil, authorization{}, slog.New(slog.NewJSONHandler(&out, nil)))

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "could not open the port") {
		t.Fatalf("log = %s", out.String())
	}
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// waitHealthy waits up to 5 s for /healthz to answer 200.
func waitHealthy(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			if closeErr := resp.Body.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s/healthz did not answer 200 within 5 s", addr)
}

func expectExit(t *testing.T, exit <-chan int, want int) {
	t.Helper()
	select {
	case code := <-exit:
		if code != want {
			t.Fatalf("exit code = %d, want %d", code, want)
		}
	case <-time.After(shutdownTimeout):
		t.Fatalf("shutdown took longer than %v", shutdownTimeout)
	}
}

func TestMigrateNeedsDatabaseURL(t *testing.T) {
	t.Setenv("ALUNA_DATABASE_URL", "")
	defer func(w io.Writer) { stderr = w }(stderr)
	var errOut bytes.Buffer
	stderr = &errOut

	code := run([]string{"migrate", "up"}, io.Discard)

	if code != 1 || !strings.Contains(errOut.String(), "ALUNA_DATABASE_URL") {
		t.Fatalf("code = %d, stderr = %q", code, errOut.String())
	}
}

func TestAPIConfigFromEnv(t *testing.T) {
	const issuer, audience = "https://iam.example/realms/aluna", "aluna-api"
	cases := map[string]struct {
		dsn, issuer, audience string
		wantErr               string
	}{
		"complete":         {"postgres://x@db/aluna", issuer, audience, ""},
		"without database": {"", issuer, audience, "ALUNA_DATABASE_URL"},
		"without issuer":   {"postgres://x@db/aluna", "", audience, "ALUNA_OIDC_ISSUER"},
		"without audience": {"postgres://x@db/aluna", issuer, "", "ALUNA_OIDC_AUDIENCE"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("ALUNA_HTTP_ADDR", "") // the default is tested without depending on the environment
			t.Setenv("ALUNA_DATABASE_URL", tc.dsn)
			t.Setenv("ALUNA_OIDC_ISSUER", tc.issuer)
			t.Setenv("ALUNA_OIDC_AUDIENCE", tc.audience)

			cfg, err := apiConfigFromEnv()

			want := apiConfig{addr: ":8080", dsn: tc.dsn, issuer: issuer, audience: audience}
			if tc.wantErr == "" && (err != nil || cfg != want) {
				t.Fatalf("cfg = %+v, err = %v", cfg, err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("error = %v, want one about %s", err, tc.wantErr)
			}
		})
	}
}
