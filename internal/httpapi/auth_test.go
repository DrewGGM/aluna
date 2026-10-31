package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/coreos/go-oidc/v3/oidc/oidctest"
	jose "github.com/go-jose/go-jose/v4"

	"github.com/DrewGGM/aluna/internal/access"
	"github.com/DrewGGM/aluna/internal/ids"
)

const testAudience = "aluna-api"

// testIssuer is a test OIDC issuer whose keys can be rotated. It counts the times the keys are requested
// (cacheable, those that do not come with Cache-Control: no-cache) and does not answer the next hangs
// requests until the client leaves.
type testIssuer struct {
	mu        sync.Mutex
	server    *oidctest.Server
	url       string
	key       *rsa.PrivateKey
	keyID     string
	keyCalls  atomic.Int32
	cacheable atomic.Int32
	hangs     atomic.Int32
}

func newTestIssuer(t *testing.T) *testIssuer {
	t.Helper()
	iss := &testIssuer{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/keys" {
			iss.keyCalls.Add(1)
			if r.Header.Get("Cache-Control") != "no-cache" {
				iss.cacheable.Add(1)
			}
			if n := iss.hangs.Load(); n > 0 && iss.hangs.CompareAndSwap(n, n-1) {
				<-r.Context().Done()
				return
			}
		}
		iss.mu.Lock()
		s := iss.server
		iss.mu.Unlock()
		s.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	iss.url = ts.URL
	iss.rotate(t)
	return iss
}

// rotate changes the issuer's key: the previous one is no longer published.
func (iss *testIssuer) rotate(t *testing.T) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := &oidctest.Server{PublicKeys: []oidctest.PublicKey{{PublicKey: key.Public(), KeyID: ids.New().String(), Algorithm: oidc.RS256}}}
	s.SetIssuer(iss.url)
	iss.mu.Lock()
	iss.server, iss.key, iss.keyID = s, key, s.PublicKeys[0].KeyID
	iss.mu.Unlock()
}

// claims are those of a token of the entity entity with the maximum lifetime, 15 min.
func (iss *testIssuer) claims(entity ids.ID) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": iss.url, "aud": testAudience, "sub": "operator-1", "iat": now.Unix(), "exp": now.Add(maxTokenLifetime).Unix(),
		"legal_entity_id": entity.String(), "roles": []string{"operator"},
	}
}

func (iss *testIssuer) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	b, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	iss.mu.Lock()
	defer iss.mu.Unlock()
	return oidctest.SignIDToken(iss.key, iss.keyID, oidc.RS256, string(b))
}

// bearerOf is the Authorization header with the token of claims signed by iss.
func (iss *testIssuer) bearerOf(t *testing.T, claims map[string]any) map[string]string {
	t.Helper()
	return map[string]string{"Authorization": "Bearer " + iss.sign(t, claims)}
}

// authenticated is a server with Authenticate that answers 200 with the request's principal.
func authenticated(t *testing.T, verifier TokenVerifier) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(Authenticate(verifier)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := access.PrincipalFrom(r.Context())
		if !ok {
			http.Error(w, "no principal", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(p) //nolint:errcheck,gosec // test response
	})))
	t.Cleanup(ts.Close)
	return ts
}

// get does GET url with header and returns the status, the response headers and, if it was 200, the
// principal the handler answered.
func get(t *testing.T, url string, header map[string]string) (int, http.Header, access.Principal) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // close in a test
	var p access.Principal
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, resp.Header, p
}

func verifierFor(t *testing.T, iss *testIssuer, now func() time.Time) *OIDCVerifier {
	t.Helper()
	return verifierLogging(t, iss, now, io.Discard)
}

func verifierLogging(t *testing.T, iss *testIssuer, now func() time.Time, logs io.Writer) *OIDCVerifier {
	t.Helper()
	v, err := newOIDCVerifier(t.Context(), iss.url, testAudience, now, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// testClock is a clock the test moves forward.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// TestAValidTokenGivesThePrincipal (FR-41, TH-14): a valid token gives the principal of its claims; the
// legal entity comes only from the token, even if the request carries a header that says otherwise.
func TestAValidTokenGivesThePrincipal(t *testing.T) {
	iss := newTestIssuer(t)
	ts := authenticated(t, verifierFor(t, iss, time.Now))
	entity, party := ids.New(), ids.New()
	header := iss.bearerOf(t, with(iss.claims(entity), "party_id", party.String()))
	header["X-Legal-Entity"], header["X-Legal-Entity-Id"] = ids.New().String(), ids.New().String()

	status, _, got := get(t, ts.URL, header)

	want := access.Principal{Subject: "operator-1", LegalEntityID: entity, PartyID: &party, Roles: []string{"operator"}}
	if status != http.StatusOK || !reflect.DeepEqual(got, want) {
		t.Fatalf("response %d with principal %+v, want %+v", status, got, want)
	}
}

// TestAnInvalidTokenIs401 (TH-1): without a token, or with one that is expired, not yet valid, without iat,
// with a future iat or a lifetime over 15 min, for another audience or from another issuer, signed with
// another key, without a subject or with an entity or party that is not a UUID, the response is 401
// problem+json.
func TestAnInvalidTokenIs401(t *testing.T) {
	iss := newTestIssuer(t)
	other := newTestIssuer(t)
	ts := authenticated(t, verifierFor(t, iss, time.Now))
	valid := func() map[string]any { return iss.claims(ids.New()) }
	cases := map[string]string{ //nolint:gosec // fake test credentials and tokens
		"no token":                "",
		"not Bearer":              "Basic dXNlcjpwYXNz",
		"expired":                 "Bearer " + iss.sign(t, with(valid(), "exp", time.Now().Add(-time.Minute).Unix())),
		"not yet valid":           "Bearer " + iss.sign(t, with(valid(), "nbf", time.Now().Add(time.Hour).Unix())),
		"no iat":                  "Bearer " + iss.sign(t, without(valid(), "iat")),
		"future iat":              "Bearer " + iss.sign(t, with(with(valid(), "iat", time.Now().Add(time.Hour).Unix()), "exp", time.Now().Add(time.Hour+maxTokenLifetime).Unix())),
		"lifetime over 15 min":    "Bearer " + iss.sign(t, with(valid(), "exp", time.Now().Add(maxTokenLifetime+time.Minute).Unix())),
		"another audience":        "Bearer " + iss.sign(t, with(valid(), "aud", "other-api")),
		"another issuer":          "Bearer " + iss.sign(t, with(valid(), "iss", other.url)),
		"signed with another key": "Bearer " + other.sign(t, valid()),
		"no legal entity":         "Bearer " + iss.sign(t, without(valid(), "legal_entity_id")),
		"entity that is not UUID": "Bearer " + iss.sign(t, with(valid(), "legal_entity_id", "entity-1")),
		"party that is not UUID":  "Bearer " + iss.sign(t, with(valid(), "party_id", "customer-1")),
		"no subject":              "Bearer " + iss.sign(t, without(valid(), "sub")),
	}
	for name, authorization := range cases {
		t.Run(name, func(t *testing.T) {
			header := map[string]string{}
			if authorization != "" {
				header["Authorization"] = authorization
			}

			status, got, _ := get(t, ts.URL, header)

			if status != http.StatusUnauthorized || got.Get("Content-Type") != "application/problem+json" || got.Get("WWW-Authenticate") == "" {
				t.Fatalf("response %d %q (WWW-Authenticate %q), want 401 problem+json with WWW-Authenticate",
					status, got.Get("Content-Type"), got.Get("WWW-Authenticate"))
			}
		})
	}
}

func with(claims map[string]any, key string, value any) map[string]any {
	claims[key] = value
	return claims
}

func without(claims map[string]any, key string) map[string]any {
	delete(claims, key)
	return claims
}

// TestProbesNeedNoToken: the orchestrator probes /healthz and /readyz without a token, and the verifier is
// not even consulted.
func TestProbesNeedNoToken(t *testing.T) {
	ts := httptest.NewServer(Authenticate(probeVerifier{t})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("{}")) //nolint:errcheck,gosec // test response
	})))
	t.Cleanup(ts.Close)

	for _, path := range []string{pathHealthz, pathReadyz} {
		if status, _, _ := get(t, ts.URL+path, nil); status != http.StatusOK {
			t.Errorf("%s without token = %d, want 200", path, status)
		}
	}
}

// probeVerifier fails the test if it is asked to verify a token.
type probeVerifier struct{ t *testing.T }

func (v probeVerifier) Verify(context.Context, string) (access.Principal, error) {
	v.t.Error("a probe went through the verifier")
	return access.Principal{}, errInvalidToken
}

// TestARetiredKeyStopsWorkingWithin10Minutes: the issuer's keys are valid for 10 min; a token signed with a
// key the issuer no longer publishes is valid while they are current (at 4 min, before the renewal at 5)
// and at 11 min it is not, even if no one renewed them.
func TestARetiredKeyStopsWorkingWithin10Minutes(t *testing.T) {
	iss := newTestIssuer(t)
	clock := &testClock{now: time.Now()}
	ts := authenticated(t, verifierFor(t, iss, clock.Now))
	token := iss.bearerOf(t, iss.claims(ids.New()))
	if status, _, _ := get(t, ts.URL, token); status != http.StatusOK {
		t.Fatalf("freshly issued token = %d", status)
	}

	iss.rotate(t)
	clock.advance(4 * time.Minute)
	if status, _, _ := get(t, ts.URL, token); status != http.StatusOK {
		t.Fatalf("at 4 min, with the key still cached = %d, want 200", status)
	}
	clock.advance(7 * time.Minute)

	if status, _, _ := get(t, ts.URL, token); status != http.StatusUnauthorized {
		t.Fatalf("at 11 min, with the key retired = %d, want 401", status)
	}
}

// TestForgedTokensDoNotFloodTheIssuer: a token the stored keys do not verify fetches them again, but at most
// once every 30 s; after that span, a new key of the issuer is learned.
func TestForgedTokensDoNotFloodTheIssuer(t *testing.T) {
	iss, forger := newTestIssuer(t), newTestIssuer(t)
	clock := &testClock{now: time.Now()}
	ts := authenticated(t, verifierFor(t, iss, clock.Now))
	if status, _, _ := get(t, ts.URL, iss.bearerOf(t, iss.claims(ids.New()))); status != http.StatusOK {
		t.Fatalf("valid token = %d", status)
	}
	calls := iss.keyCalls.Load()
	clock.advance(jwksMinRefetch / 3)

	forged := forger.bearerOf(t, iss.claims(ids.New()))
	for range 20 {
		if status, _, _ := get(t, ts.URL, forged); status != http.StatusUnauthorized {
			t.Fatalf("forged token = %d, want 401", status)
		}
	}
	if got := iss.keyCalls.Load() - calls; got != 0 {
		t.Fatalf("20 forged tokens 10 s after fetching the keys fetched them %d more times, want 0", got)
	}

	iss.rotate(t)
	clock.advance(jwksMinRefetch)
	if status, _, _ := get(t, ts.URL, iss.bearerOf(t, iss.claims(ids.New()))); status != http.StatusOK {
		t.Fatalf("token with the new key, 30 s later = %d, want 200", status)
	}
	if n := iss.cacheable.Load(); n != 0 {
		t.Fatalf("%d downloads without Cache-Control: no-cache; after a rotation a proxy would serve the old JWKS", n)
	}
}

// TestAHungIssuerIsCutAndLogged: a key download the issuer does not answer is cut at 5 s and logged; with no
// current keys, it is retried after a second and the token is valid.
func TestAHungIssuerIsCutAndLogged(t *testing.T) {
	iss := newTestIssuer(t)
	clock := &testClock{now: time.Now()}
	var logs syncBuffer
	ts := authenticated(t, verifierLogging(t, iss, clock.Now, &logs))
	token := iss.bearerOf(t, iss.claims(ids.New()))
	iss.hangs.Store(1)

	start := time.Now()
	status, _, _ := get(t, ts.URL, token)
	if took := time.Since(start); status != http.StatusUnauthorized || took < issuerTimeout || took > issuerTimeout+time.Second {
		t.Fatalf("with the issuer hung = %d in %s, want 401 in about %s", status, took, issuerTimeout)
	}
	if !strings.Contains(logs.String(), "fetch OIDC issuer keys") {
		t.Fatalf("the issuer failure was not logged:\n%s", logs.String())
	}

	clock.advance(jwksRetry)
	if status, _, _ := get(t, ts.URL, token); status != http.StatusOK {
		t.Fatalf("with the issuer back, 1 s later = %d, want 200", status)
	}
}

// TestAFetchDoesNotHoldBackCachedTokens: while a key download waits on a hung issuer, a token the stored keys
// verify does not wait for it.
func TestAFetchDoesNotHoldBackCachedTokens(t *testing.T) {
	iss, forger := newTestIssuer(t), newTestIssuer(t)
	clock := &testClock{now: time.Now()}
	keys := keysOf(iss, clock.Now, time.Second)
	valid := iss.sign(t, iss.claims(ids.New()))
	if _, err := keys.VerifySignature(t.Context(), valid); err != nil {
		t.Fatal(err)
	}
	clock.advance(jwksMinRefetch)
	iss.hangs.Store(1)
	calls := iss.keyCalls.Load()
	forged := make(chan error, 1)
	go func() {
		_, err := keys.VerifySignature(context.Background(), forger.sign(t, iss.claims(ids.New())))
		forged <- err
	}()
	for deadline := time.Now().Add(2 * time.Second); iss.keyCalls.Load() == calls; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the forged token did not fetch the keys")
		}
	}

	start := time.Now()
	_, err := keys.VerifySignature(t.Context(), valid)

	if took := time.Since(start); err != nil || took > 200*time.Millisecond {
		t.Fatalf("cached token during a hung download = %v in %s, want valid without waiting for it", err, took)
	}
	if err := <-forged; err == nil {
		t.Fatal("the forged token was verified")
	}
}

// TestKeysAreRenewedBeforeTheyExpire: past half of their life, a token they verify renews the keys in the
// background; 10 min after the first download they are still current without fetching them again.
func TestKeysAreRenewedBeforeTheyExpire(t *testing.T) {
	iss := newTestIssuer(t)
	clock := &testClock{now: time.Now()}
	keys := keysOf(iss, clock.Now, issuerTimeout)
	jwt := iss.sign(t, iss.claims(ids.New()))
	if _, err := keys.VerifySignature(t.Context(), jwt); err != nil {
		t.Fatal(err)
	}

	clock.advance(6 * time.Minute)
	if _, err := keys.VerifySignature(t.Context(), jwt); err != nil {
		t.Fatal(err)
	}
	keys.fetching.Lock() // waits for the background renewal
	renewed := iss.keyCalls.Load()
	keys.fetching.Unlock()
	if renewed != 2 {
		t.Fatalf("at 6 min the keys were fetched %d times in total, want 2", renewed)
	}
	clock.advance(4 * time.Minute)

	_, err := keys.VerifySignature(t.Context(), jwt)

	if err != nil || iss.keyCalls.Load() != renewed {
		t.Fatalf("at 10 min, with the keys renewed at 6 = %v after %d downloads, want valid without downloading",
			err, iss.keyCalls.Load())
	}
}

// TestAFailedRenewalWaitsBeforeRetrying: a background renewal that fails is not repeated with every token
// until 30 s have passed.
func TestAFailedRenewalWaitsBeforeRetrying(t *testing.T) {
	iss := newTestIssuer(t)
	clock := &testClock{now: time.Now()}
	keys := keysOf(iss, clock.Now, 100*time.Millisecond)
	jwt := iss.sign(t, iss.claims(ids.New()))
	if _, err := keys.VerifySignature(t.Context(), jwt); err != nil {
		t.Fatal(err)
	}
	clock.advance(6 * time.Minute)
	iss.hangs.Store(1)
	if _, err := keys.VerifySignature(t.Context(), jwt); err != nil {
		t.Fatal(err)
	}
	keys.fetching.Lock() // waits for the failed renewal
	failed := iss.keyCalls.Load()
	keys.fetching.Unlock()

	for range 5 {
		if _, err := keys.VerifySignature(t.Context(), jwt); err != nil {
			t.Fatal(err)
		}
	}

	keys.fetching.Lock()
	defer keys.fetching.Unlock()
	if failed != 2 || iss.keyCalls.Load() != failed {
		t.Fatalf("downloads = %d after the failed renewal and %d afterwards, want 2 and no more", failed, iss.keyCalls.Load())
	}
}

// TestAHungIssuerIsAskedOnce: with no current keys and the issuer hung, the requests waiting on the download
// do not repeat it one after another: there is only one and they all finish with it.
func TestAHungIssuerIsAskedOnce(t *testing.T) {
	iss := newTestIssuer(t)
	const timeout = jwksRetry + 200*time.Millisecond // more than jwksRetry, like issuerTimeout
	keys := keysOf(iss, time.Now, timeout)
	jwt := iss.sign(t, iss.claims(ids.New()))
	iss.hangs.Store(100)

	start := time.Now()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if _, err := keys.VerifySignature(context.Background(), jwt); err == nil {
				t.Error("with the issuer hung, a signature was verified")
			}
		})
	}
	wg.Wait()

	if took := time.Since(start); iss.keyCalls.Load() != 1 || took > 2*timeout {
		t.Fatalf("4 requests without keys = %d downloads in %s, want 1 in about %s", iss.keyCalls.Load(), took, timeout)
	}
}

// keysOf are the keys of iss with the clock now and a timeout per download.
func keysOf(iss *testIssuer, now func() time.Time, timeout time.Duration) *issuerKeys {
	return &issuerKeys{
		url: iss.url + "/keys", client: &http.Client{Timeout: timeout}, now: now,
		log: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
}

// TestAResponseWithoutKeysIsAnError: a 200 that is not a JWKS does not count as an empty key set.
func TestAResponseWithoutKeysIsAnError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"error":"starting"}`)) //nolint:errcheck,gosec // test response
	}))
	t.Cleanup(ts.Close)

	if _, err := fetchKeys(t.Context(), http.DefaultClient, ts.URL); !errors.Is(err, errBadJWKS) {
		t.Fatalf("= %v, want errBadJWKS", err)
	}
}

// TestUnreadableAndEncryptionKeysAreSkipped: only the readable signing keys are kept from the JWKS; a key of
// another curve (Ed448) or an encryption key does not break the rest.
func TestUnreadableAndEncryptionKeysAreSkipped(t *testing.T) {
	sig, enc := jwkOf(t, "sig"), jwkOf(t, "enc")
	ed448 := `{"kty":"OKP","crv":"Ed448","alg":"EdDSA","use":"sig","x":"` + strings.Repeat("A", 76) + `"}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"keys":[` + ed448 + `,` + enc.json + `,` + sig.json + `]}`)) //nolint:errcheck,gosec // test response
	}))
	t.Cleanup(ts.Close)

	keys, err := fetchKeys(t.Context(), http.DefaultClient, ts.URL)

	if err != nil || len(keys.PublicKeys) != 1 || !sig.key.Equal(keys.PublicKeys[0]) {
		t.Fatalf("keys = %v, %v; want only the signing one", keys, err)
	}
}

// testJWK is a public key and its JWK.
type testJWK struct {
	key  *rsa.PublicKey
	json string
}

// jwkOf is a new public RSA key with use use.
func jwkOf(t *testing.T, use string) testJWK {
	t.Helper()
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(jose.JSONWebKey{Key: private.Public(), KeyID: ids.New().String(), Algorithm: oidc.RS256, Use: use})
	if err != nil {
		t.Fatal(err)
	}
	return testJWK{key: &private.PublicKey, json: string(b)}
}

// syncBuffer is a bytes.Buffer the server writes and the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestAnIssuerWithoutJWKSIsRejected: an issuer that does not publish jwks_uri cannot verify tokens.
func TestAnIssuerWithoutJWKSIsRejected(t *testing.T) {
	var issuer string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": issuer}) //nolint:errcheck,gosec // test response
	}))
	t.Cleanup(ts.Close)
	issuer = ts.URL

	_, err := newOIDCVerifier(t.Context(), issuer, testAudience, time.Now, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	if !errors.Is(err, errNoJWKS) {
		t.Fatalf("issuer without jwks_uri = %v, want errNoJWKS", err)
	}
}
