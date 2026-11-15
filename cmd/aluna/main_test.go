package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/httpapi"
	"github.com/DrewGGM/aluna/internal/payments"
	"github.com/DrewGGM/aluna/internal/pricing"
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
		"healthcheck with arguments":   {"healthcheck", "extra"},
		"audit without subcommand":     {"audit"},
		"unknown audit subcommand":     {"audit", "check"},
		"verify without blocks":        {"audit", "verify"},
		"verify reversed range":        {"audit", "verify", "--from", "3", "--to", "2"},
		"verify with extra arguments":  {"audit", "verify", "--from", "1", "--to", "2", "extra"},
		"entity without subcommand":    {"entity"},
		"unknown entity subcommand":    {"entity", "delete"},
		"create without name":          {"entity", "create", "--business-date", "2026-10-30", "--currency", "COP"},
		"create without business date": {"entity", "create", "--name", "Entity", "--currency", "COP"},
		"create without currency":      {"entity", "create", "--name", "Entity", "--business-date", "2026-10-30"},
		"create with extra arguments":  {"entity", "create", "--name", "Entity", "--business-date", "2026-10-30", "--currency", "COP", "extra"},
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
		exit <- serveAPI(ctx, quietLogger(), serving{ln, httpapi.NewServer(httpapi.ServerConfig{}, nil, quietLogger())})
	}()

	waitHealthy(t, ln.Addr().String())
	cancel()

	expectExit(t, exit, 0)
}

// TestServeAPIStopsWhenAServerStops: if one server stops on its own, the others shut down and the api ends
// with an error.
func TestServeAPIStopsWhenAServerStops(t *testing.T) {
	var lns [2]net.Listener
	for i := range lns {
		ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		lns[i] = ln
	}
	exit := make(chan int, 1)
	go func() {
		exit <- serveAPI(t.Context(), quietLogger(), serving{lns[0], httpapi.NewServer(httpapi.ServerConfig{}, nil, quietLogger())},
			serving{lns[1], httpapi.NewServer(httpapi.ServerConfig{}, nil, quietLogger())})
	}()
	waitHealthy(t, lns[1].Addr().String())

	if err := lns[0].Close(); err != nil {
		t.Fatal(err)
	}

	expectExit(t, exit, 1)
	if _, err := http.Get("http://" + lns[1].Addr().String() + "/healthz"); err == nil { //nolint:bodyclose,noctx // it must not connect
		t.Fatal("the other server still serves")
	}
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

// TestEntityCreateChecksItsFlags: a business date that is not yyyy-mm-dd or an ID that is not a UUID exits with 1
// before connecting to the database.
func TestEntityCreateChecksItsFlags(t *testing.T) {
	t.Setenv("ALUNA_DATABASE_URL", "postgres://nobody@127.0.0.1:1/aluna")
	cases := map[string][]string{
		"--business-date": {"--business-date", "30/10/2026", "--currency", "COP"},
		"--id":            {"--id", "123", "--business-date", "2026-10-30", "--currency", "COP"},
	}
	for flag, args := range cases {
		t.Run(flag, func(t *testing.T) {
			defer func(w io.Writer) { stderr = w }(stderr)
			var errOut bytes.Buffer
			stderr = &errOut

			code := run(append([]string{"entity", "create", "--name", "Entity"}, args...), io.Discard)

			if code != 1 || !strings.Contains(errOut.String(), flag) {
				t.Fatalf("code = %d, stderr = %q", code, errOut.String())
			}
		})
	}
}

// railSecret is a secret of the simulated rail as long as the shortest it takes.
const railSecret = "a secret of the simulated rail.."

// expectListScreening checks that the environment configures the lists' screening: the api, the defaults and the
// identity service's token; the worker, the UN list of the mirror and the others the official ones.
func expectListScreening(t *testing.T) {
	t.Helper()
	l, err := listScreeningFromEnv()
	if err != nil || l.Threshold != 0.92 || l.MaxAge != 48*time.Hour || l.IdentityToken != "t0k3n" {
		t.Fatalf("screening = %+v (%v)", l, err)
	}
	s, err := listSourcesFromEnv()
	if err != nil || s.UN != "https://mirror.example/consolidated.xml" || !strings.HasPrefix(s.OFACSDN, "https://sanctionslistservice.ofac.treas.gov/") {
		t.Fatalf("sources = %+v (%v)", s, err)
	}
}

// TestTheScreeningComesFromTheEnvironment (ADR-0018): without ALUNA_SCREENING_VENDOR the vendor is the fake one;
// with lists, the api takes the identity service, the threshold and the maximum age, and the worker the official
// lists unless a URL says otherwise; anything else is refused.
func TestTheScreeningComesFromTheEnvironment(t *testing.T) {
	for _, key := range []string{
		"ALUNA_SCREENING_VENDOR", "ALUNA_SCREENING_IDENTITY_URL", "ALUNA_SCREENING_IDENTITY_TOKEN", "ALUNA_SCREENING_THRESHOLD",
		"ALUNA_SCREENING_MAX_LIST_AGE_HOURS", "ALUNA_SCREENING_OFAC_SDN_URL", "ALUNA_SCREENING_OFAC_ALT_URL", "ALUNA_SCREENING_UN_URL",
		"ALUNA_SCREENING_PEP_URL",
	} {
		t.Setenv(key, "")
	}
	l, err := listScreeningFromEnv()
	s, err2 := listSourcesFromEnv()
	if err != nil || err2 != nil || l != nil || s != nil {
		t.Fatalf("the default = %+v, %+v (%v, %v), want the fake vendor", l, s, err, err2)
	}

	t.Setenv("ALUNA_SCREENING_VENDOR", "lists")
	t.Setenv("ALUNA_SCREENING_IDENTITY_URL", "https://onboarding.example/identity")
	t.Setenv("ALUNA_SCREENING_IDENTITY_TOKEN", "t0k3n")
	t.Setenv("ALUNA_SCREENING_UN_URL", "https://mirror.example/consolidated.xml")
	expectListScreening(t)

	for key, value := range map[string]string{
		"ALUNA_SCREENING_VENDOR": "vendorx", "ALUNA_SCREENING_THRESHOLD": "high", "ALUNA_SCREENING_MAX_LIST_AGE_HOURS": "two",
	} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, value)
			if _, err := listScreeningFromEnv(); !errors.Is(err, errScreening) {
				t.Fatalf("%s=%s = %v, want errScreening", key, value, err)
			}
		})
	}
	t.Setenv("ALUNA_SCREENING_VENDOR", "vendorx")
	if _, err := listSourcesFromEnv(); !errors.Is(err, errScreening) {
		t.Fatalf("the worker with vendorx = %v, want errScreening", err)
	}
}

// TestTheAuditAnchorComesFromTheEnvironment (ADR-0017): the anchor is a directory or an S3 bucket, exactly one of
// them; a bucket needs its keys and a retention of whole days, from one to a century.
func TestTheAuditAnchorComesFromTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]struct {
		dir, bucket, keyID, retention string
		refused                       bool
		because                       error // why it is refused, when cmd/aluna says it
	}{
		"a directory":           {dir: dir},
		"a bucket":              {bucket: "https://anchors.s3.us-east-1.amazonaws.com", keyID: "AKID"},
		"neither":               {refused: true, because: errAnchorChoice},
		"both":                  {dir: dir, bucket: "https://host/anchors", keyID: "AKID", refused: true, because: errAnchorChoice},
		"no whole days":         {bucket: "https://host/anchors", keyID: "AKID", retention: "1.5", refused: true, because: errAnchorRetention},
		"no days":               {bucket: "https://host/anchors", keyID: "AKID", retention: "0", refused: true, because: errAnchorRetention},
		"over a century":        {bucket: "https://host/anchors", keyID: "AKID", retention: "213504", refused: true, because: errAnchorRetention},
		"a bucket without keys": {bucket: "https://host/anchors", refused: true}, // refused by the bucket adapter's validation
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("ALUNA_AUDIT_ANCHOR_DIR", tc.dir)
			t.Setenv("ALUNA_AUDIT_ANCHOR_S3_URL", tc.bucket)
			t.Setenv("ALUNA_AUDIT_ANCHOR_RETENTION_DAYS", tc.retention)
			t.Setenv("AWS_ACCESS_KEY_ID", tc.keyID)
			t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

			_, err := auditFromEnv()

			if (err != nil) != tc.refused {
				t.Fatalf("error = %v, want it refused: %t", err, tc.refused)
			}
			if tc.because != nil && !errors.Is(err, tc.because) {
				t.Fatalf("error = %v, want %v", err, tc.because)
			}
		})
	}
}

// TestThePaymentsPolicyComesFromTheEnvironment (ADR-0016): without its variables the policy is the default; each
// one changes its part, and a value that is not one of the allowed is an error naming the variable.
func TestThePaymentsPolicyComesFromTheEnvironment(t *testing.T) {
	keep := payments.DefaultPolicy()
	keep.RefundFeesOnReturn, keep.AnnullingReasons, keep.RestoreExemption, keep.HonorQuotes = false, []string{"AC01", "AC04"}, false, true
	none := payments.DefaultPolicy()
	none.AnnullingReasons = nil
	cases := map[string]struct {
		fees, reasons, restore, quotes string
		unsetReasons                   bool
		want                           payments.Policy
		wantErr                        string
	}{
		"the default":           {unsetReasons: true, want: payments.DefaultPolicy()},
		"each one changed":      {fees: "false", reasons: "AC01, AC04", restore: "never", quotes: "honor", want: keep},
		"no reason refunds GMF": {reasons: "", want: none},
		"fees neither":          {fees: "maybe", unsetReasons: true, wantErr: "ALUNA_RETURN_REFUND_FEES"},
		"fees as a number":      {fees: "1", unsetReasons: true, wantErr: "ALUNA_RETURN_REFUND_FEES"},
		"a reason of no code":   {reasons: "AC01,account closed", wantErr: "ALUNA_RETURN_REFUND_GMF_REASONS"},
		"restore neither":       {unsetReasons: true, restore: "always", wantErr: "ALUNA_GMF_RESTORE_EXEMPTION"},
		"quotes neither":        {unsetReasons: true, quotes: "requote", wantErr: "ALUNA_QUOTE_POLICY"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("ALUNA_RETURN_REFUND_FEES", tc.fees)
			t.Setenv("ALUNA_GMF_RESTORE_EXEMPTION", tc.restore)
			t.Setenv("ALUNA_QUOTE_POLICY", tc.quotes)
			t.Setenv("ALUNA_RETURN_REFUND_GMF_REASONS", tc.reasons)
			if tc.unsetReasons {
				unsetenv(t, "ALUNA_RETURN_REFUND_GMF_REASONS")
			}

			got, err := paymentsPolicyFromEnv()

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want one about %s", err, tc.wantErr)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("policy = %+v (%v), want %+v", got, err, tc.want)
			}
		})
	}
}

// unsetenv unsets key for the test, after t.Setenv registered its restoration.
func unsetenv(t *testing.T, key string) {
	t.Helper()
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

func TestAPIConfigFromEnv(t *testing.T) {
	const issuer, audience = "https://iam.example/realms/aluna", "aluna-api"
	cases := map[string]struct {
		dsn, issuer, audience, gmf, secret string
		wantGMF                            pricing.Modality
		wantErr                            string
	}{
		"complete":              {"postgres://x@db/aluna", issuer, audience, "", railSecret, pricing.SingleAccount, ""},
		"split GMF exemption":   {"postgres://x@db/aluna", issuer, audience, "split_accounts", railSecret, pricing.SplitAccounts, ""},
		"without database":      {"", issuer, audience, "", railSecret, "", "ALUNA_DATABASE_URL"},
		"without issuer":        {"postgres://x@db/aluna", "", audience, "", railSecret, "", "ALUNA_OIDC_ISSUER"},
		"without audience":      {"postgres://x@db/aluna", issuer, "", "", railSecret, "", "ALUNA_OIDC_AUDIENCE"},
		"unknown GMF exemption": {"postgres://x@db/aluna", issuer, audience, "both", railSecret, "", "unknown modality"},
		"without rail secret":   {"postgres://x@db/aluna", issuer, audience, "", "", "", "ALUNA_SIMBREB_SECRET"},
		"a short rail secret":   {"postgres://x@db/aluna", issuer, audience, "", railSecret[1:], "", "ALUNA_SIMBREB_SECRET"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("ALUNA_HTTP_ADDR", "") // the defaults are tested without depending on the environment
			t.Setenv("ALUNA_RAIL_ADDR", "")
			t.Setenv("ALUNA_DATABASE_URL", tc.dsn)
			t.Setenv("ALUNA_OIDC_ISSUER", tc.issuer)
			t.Setenv("ALUNA_OIDC_AUDIENCE", tc.audience)
			t.Setenv("ALUNA_GMF_MODALITY", tc.gmf)
			t.Setenv("ALUNA_SIMBREB_SECRET", tc.secret)
			t.Setenv("ALUNA_RETURN_REFUND_FEES", "") // the default policy, whatever the environment says
			t.Setenv("ALUNA_GMF_RESTORE_EXEMPTION", "")
			t.Setenv("ALUNA_QUOTE_POLICY", "")
			t.Setenv("ALUNA_RETURN_REFUND_GMF_REASONS", "")
			unsetenv(t, "ALUNA_RETURN_REFUND_GMF_REASONS")

			cfg, err := apiConfigFromEnv()

			want := apiConfig{
				addr: ":8080", railAddr: ":8081", dsn: tc.dsn, issuer: issuer, audience: audience,
				charging: charging{gmf: tc.wantGMF, payments: payments.DefaultPolicy()}, railSecret: railSecret,
			}
			if tc.wantErr == "" && (err != nil || !reflect.DeepEqual(cfg, want)) {
				t.Fatalf("cfg = %+v, err = %v, want %+v", cfg, err, want)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("error = %v, want one about %s", err, tc.wantErr)
			}
		})
	}
}
