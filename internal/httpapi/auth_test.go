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

// testIssuer es un emisor OIDC de prueba cuyas claves se pueden rotar. Cuenta las veces que le piden las
// claves (cacheable, las que no llegan con Cache-Control: no-cache) y no responde las siguientes hangs
// hasta que el cliente se va.
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

// rotate cambia la clave del emisor: la anterior deja de publicarse.
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

// claims son los de un token de la entidad entity con la vida máxima, 15 min.
func (iss *testIssuer) claims(entity ids.ID) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": iss.url, "aud": testAudience, "sub": "operador-1", "iat": now.Unix(), "exp": now.Add(maxTokenLifetime).Unix(),
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

// bearerOf es el header Authorization con el token de claims firmado por iss.
func (iss *testIssuer) bearerOf(t *testing.T, claims map[string]any) map[string]string {
	t.Helper()
	return map[string]string{"Authorization": "Bearer " + iss.sign(t, claims)}
}

// authenticated es un servidor con Authenticate que responde 200 con el principal del request.
func authenticated(t *testing.T, verifier TokenVerifier) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(Authenticate(verifier)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := access.PrincipalFrom(r.Context())
		if !ok {
			http.Error(w, "sin principal", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(p) //nolint:errcheck,gosec // respuesta de prueba
	})))
	t.Cleanup(ts.Close)
	return ts
}

// get hace GET url con header y devuelve el status, los headers de la respuesta y, si fue 200, el
// principal que respondió el handler.
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
	defer resp.Body.Close() //nolint:errcheck // cierre en prueba
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

// testClock es un reloj que la prueba adelanta.
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

// TestAValidTokenGivesThePrincipal (FR-41, TH-14): un token válido da el principal de sus claims; la
// entidad legal sale solo del token, aunque el request traiga un header que diga otra.
func TestAValidTokenGivesThePrincipal(t *testing.T) {
	iss := newTestIssuer(t)
	ts := authenticated(t, verifierFor(t, iss, time.Now))
	entity, party := ids.New(), ids.New()
	header := iss.bearerOf(t, with(iss.claims(entity), "party_id", party.String()))
	header["X-Legal-Entity"], header["X-Legal-Entity-Id"] = ids.New().String(), ids.New().String()

	status, _, got := get(t, ts.URL, header)

	want := access.Principal{Subject: "operador-1", LegalEntityID: entity, PartyID: &party, Roles: []string{"operator"}}
	if status != http.StatusOK || !reflect.DeepEqual(got, want) {
		t.Fatalf("respuesta %d con principal %+v, se esperaba %+v", status, got, want)
	}
}

// TestAnInvalidTokenIs401 (TH-1): sin token, o con uno vencido, aún no válido, sin iat, con iat futuro o de
// vida mayor a 15 min, de otra audiencia u otro emisor, firmado con otra clave, sin sujeto o con entidad o
// parte que no son UUID, la respuesta es 401 problem+json.
func TestAnInvalidTokenIs401(t *testing.T) {
	iss := newTestIssuer(t)
	other := newTestIssuer(t)
	ts := authenticated(t, verifierFor(t, iss, time.Now))
	valid := func() map[string]any { return iss.claims(ids.New()) }
	cases := map[string]string{ //nolint:gosec // credenciales y tokens falsos de prueba
		"sin token":              "",
		"no es Bearer":           "Basic dXNlcjpwYXNz",
		"vencido":                "Bearer " + iss.sign(t, with(valid(), "exp", time.Now().Add(-time.Minute).Unix())),
		"aún no válido":          "Bearer " + iss.sign(t, with(valid(), "nbf", time.Now().Add(time.Hour).Unix())),
		"sin iat":                "Bearer " + iss.sign(t, without(valid(), "iat")),
		"iat futuro":             "Bearer " + iss.sign(t, with(with(valid(), "iat", time.Now().Add(time.Hour).Unix()), "exp", time.Now().Add(time.Hour+maxTokenLifetime).Unix())),
		"vida de más de 15 min":  "Bearer " + iss.sign(t, with(valid(), "exp", time.Now().Add(maxTokenLifetime+time.Minute).Unix())),
		"otra audiencia":         "Bearer " + iss.sign(t, with(valid(), "aud", "otra-api")),
		"otro emisor":            "Bearer " + iss.sign(t, with(valid(), "iss", other.url)),
		"firma de otra clave":    "Bearer " + other.sign(t, valid()),
		"sin entidad legal":      "Bearer " + iss.sign(t, without(valid(), "legal_entity_id")),
		"entidad que no es UUID": "Bearer " + iss.sign(t, with(valid(), "legal_entity_id", "entidad-1")),
		"parte que no es UUID":   "Bearer " + iss.sign(t, with(valid(), "party_id", "cliente-1")),
		"sin sujeto":             "Bearer " + iss.sign(t, without(valid(), "sub")),
	}
	for name, authorization := range cases {
		t.Run(name, func(t *testing.T) {
			header := map[string]string{}
			if authorization != "" {
				header["Authorization"] = authorization
			}

			status, got, _ := get(t, ts.URL, header)

			if status != http.StatusUnauthorized || got.Get("Content-Type") != "application/problem+json" || got.Get("WWW-Authenticate") == "" {
				t.Fatalf("respuesta %d %q (WWW-Authenticate %q), se esperaba 401 problem+json con WWW-Authenticate",
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

// TestProbesNeedNoToken: el orquestador sondea /healthz y /readyz sin token, y el verificador ni se
// consulta.
func TestProbesNeedNoToken(t *testing.T) {
	ts := httptest.NewServer(Authenticate(probeVerifier{t})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("{}")) //nolint:errcheck,gosec // respuesta de prueba
	})))
	t.Cleanup(ts.Close)

	for _, path := range []string{pathHealthz, pathReadyz} {
		if status, _, _ := get(t, ts.URL+path, nil); status != http.StatusOK {
			t.Errorf("%s sin token = %d, se esperaba 200", path, status)
		}
	}
}

// probeVerifier falla la prueba si se le pide verificar un token.
type probeVerifier struct{ t *testing.T }

func (v probeVerifier) Verify(context.Context, string) (access.Principal, error) {
	v.t.Error("un probe pasó por el verificador")
	return access.Principal{}, errInvalidToken
}

// TestARetiredKeyStopsWorkingWithin10Minutes: las claves del emisor valen 10 min; un token firmado con una
// clave que el emisor ya no publica vale mientras siguen vigentes (a los 4 min, antes de la renovación de
// los 5) y a los 11 min ya no, aunque nadie las haya renovado.
func TestARetiredKeyStopsWorkingWithin10Minutes(t *testing.T) {
	iss := newTestIssuer(t)
	clock := &testClock{now: time.Now()}
	ts := authenticated(t, verifierFor(t, iss, clock.Now))
	token := iss.bearerOf(t, iss.claims(ids.New()))
	if status, _, _ := get(t, ts.URL, token); status != http.StatusOK {
		t.Fatalf("token recién emitido = %d", status)
	}

	iss.rotate(t)
	clock.advance(4 * time.Minute)
	if status, _, _ := get(t, ts.URL, token); status != http.StatusOK {
		t.Fatalf("a los 4 min, con la clave aún en caché = %d, se esperaba 200", status)
	}
	clock.advance(7 * time.Minute)

	if status, _, _ := get(t, ts.URL, token); status != http.StatusUnauthorized {
		t.Fatalf("a los 11 min, con la clave retirada = %d, se esperaba 401", status)
	}
}

// TestForgedTokensDoNotFloodTheIssuer: un token que las claves guardadas no verifican las vuelve a pedir,
// pero a lo sumo una vez cada 30 s; pasado ese lapso, una clave nueva del emisor se aprende.
func TestForgedTokensDoNotFloodTheIssuer(t *testing.T) {
	iss, forger := newTestIssuer(t), newTestIssuer(t)
	clock := &testClock{now: time.Now()}
	ts := authenticated(t, verifierFor(t, iss, clock.Now))
	if status, _, _ := get(t, ts.URL, iss.bearerOf(t, iss.claims(ids.New()))); status != http.StatusOK {
		t.Fatalf("token válido = %d", status)
	}
	calls := iss.keyCalls.Load()
	clock.advance(jwksMinRefetch / 3)

	forged := forger.bearerOf(t, iss.claims(ids.New()))
	for range 20 {
		if status, _, _ := get(t, ts.URL, forged); status != http.StatusUnauthorized {
			t.Fatalf("token falsificado = %d, se esperaba 401", status)
		}
	}
	if got := iss.keyCalls.Load() - calls; got != 0 {
		t.Fatalf("20 tokens falsos 10 s después de pedir las claves las pidieron %d veces más, se esperaba 0", got)
	}

	iss.rotate(t)
	clock.advance(jwksMinRefetch)
	if status, _, _ := get(t, ts.URL, iss.bearerOf(t, iss.claims(ids.New()))); status != http.StatusOK {
		t.Fatalf("token con la clave nueva, 30 s después = %d, se esperaba 200", status)
	}
	if n := iss.cacheable.Load(); n != 0 {
		t.Fatalf("%d descargas sin Cache-Control: no-cache; tras una rotación un proxy daría el JWKS viejo", n)
	}
}

// TestAHungIssuerIsCutAndLogged: una descarga de claves que el emisor no responde se corta a los 5 s y
// queda en log; sin claves vigentes, se vuelve a intentar al segundo y el token vale.
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
		t.Fatalf("con el emisor colgado = %d en %s, se esperaba 401 en unos %s", status, took, issuerTimeout)
	}
	if !strings.Contains(logs.String(), "pedir las claves del emisor OIDC") {
		t.Fatalf("la falla del emisor no quedó en log:\n%s", logs.String())
	}

	clock.advance(jwksRetry)
	if status, _, _ := get(t, ts.URL, token); status != http.StatusOK {
		t.Fatalf("con el emisor de vuelta, 1 s después = %d, se esperaba 200", status)
	}
}

// TestAFetchDoesNotHoldBackCachedTokens: mientras una descarga de claves espera a un emisor colgado, un token
// que las claves guardadas verifican no la espera.
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
			t.Fatal("el token falsificado no pidió las claves")
		}
	}

	start := time.Now()
	_, err := keys.VerifySignature(t.Context(), valid)

	if took := time.Since(start); err != nil || took > 200*time.Millisecond {
		t.Fatalf("token en caché durante una descarga colgada = %v en %s, se esperaba válido sin esperarla", err, took)
	}
	if err := <-forged; err == nil {
		t.Fatal("el token falsificado se verificó")
	}
}

// TestKeysAreRenewedBeforeTheyExpire: pasada la mitad de su vida, un token que verifican renueva las claves
// en segundo plano; a los 10 min de la primera descarga siguen vigentes sin volver a pedirlas.
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
	keys.fetching.Lock() // espera la renovación en segundo plano
	renewed := iss.keyCalls.Load()
	keys.fetching.Unlock()
	if renewed != 2 {
		t.Fatalf("a los 6 min las claves se pidieron %d veces en total, se esperaba 2", renewed)
	}
	clock.advance(4 * time.Minute)

	_, err := keys.VerifySignature(t.Context(), jwt)

	if err != nil || iss.keyCalls.Load() != renewed {
		t.Fatalf("a los 10 min, con las claves renovadas a los 6 = %v tras %d descargas, se esperaba válida sin descargar",
			err, iss.keyCalls.Load())
	}
}

// TestAFailedRenewalWaitsBeforeRetrying: una renovación en segundo plano que falla no se repite con cada
// token hasta pasados 30 s.
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
	keys.fetching.Lock() // espera la renovación fallida
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
		t.Fatalf("descargas = %d tras la renovación fallida y %d después, se esperaba 2 y ninguna más", failed, iss.keyCalls.Load())
	}
}

// TestAHungIssuerIsAskedOnce: sin claves vigentes y con el emisor colgado, los requests que esperan la
// descarga no la repiten uno tras otro: hay una sola y todos terminan con ella.
func TestAHungIssuerIsAskedOnce(t *testing.T) {
	iss := newTestIssuer(t)
	const timeout = jwksRetry + 200*time.Millisecond // más que jwksRetry, como issuerTimeout
	keys := keysOf(iss, time.Now, timeout)
	jwt := iss.sign(t, iss.claims(ids.New()))
	iss.hangs.Store(100)

	start := time.Now()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if _, err := keys.VerifySignature(context.Background(), jwt); err == nil {
				t.Error("con el emisor colgado, una firma se verificó")
			}
		})
	}
	wg.Wait()

	if took := time.Since(start); iss.keyCalls.Load() != 1 || took > 2*timeout {
		t.Fatalf("4 requests sin claves = %d descargas en %s, se esperaba 1 en unos %s", iss.keyCalls.Load(), took, timeout)
	}
}

// keysOf son las claves de iss con el reloj now y un timeout por descarga.
func keysOf(iss *testIssuer, now func() time.Time, timeout time.Duration) *issuerKeys {
	return &issuerKeys{
		url: iss.url + "/keys", client: &http.Client{Timeout: timeout}, now: now,
		log: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
}

// TestAResponseWithoutKeysIsAnError: un 200 que no es un JWKS no cuenta como un juego de claves vacío.
func TestAResponseWithoutKeysIsAnError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"error":"arrancando"}`)) //nolint:errcheck,gosec // respuesta de prueba
	}))
	t.Cleanup(ts.Close)

	if _, err := fetchKeys(t.Context(), http.DefaultClient, ts.URL); !errors.Is(err, errBadJWKS) {
		t.Fatalf("= %v, se esperaba errBadJWKS", err)
	}
}

// TestUnreadableAndEncryptionKeysAreSkipped: del JWKS quedan solo las claves de firma que se pueden leer;
// una clave de otra curva (Ed448) o de cifrado no tumba las demás.
func TestUnreadableAndEncryptionKeysAreSkipped(t *testing.T) {
	sig, enc := jwkOf(t, "sig"), jwkOf(t, "enc")
	ed448 := `{"kty":"OKP","crv":"Ed448","alg":"EdDSA","use":"sig","x":"` + strings.Repeat("A", 76) + `"}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"keys":[` + ed448 + `,` + enc.json + `,` + sig.json + `]}`)) //nolint:errcheck,gosec // respuesta de prueba
	}))
	t.Cleanup(ts.Close)

	keys, err := fetchKeys(t.Context(), http.DefaultClient, ts.URL)

	if err != nil || len(keys.PublicKeys) != 1 || !sig.key.Equal(keys.PublicKeys[0]) {
		t.Fatalf("claves = %v, %v; se esperaba solo la de firma", keys, err)
	}
}

// testJWK es una clave pública y su JWK.
type testJWK struct {
	key  *rsa.PublicKey
	json string
}

// jwkOf es una clave RSA pública nueva con uso use.
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

// syncBuffer es un bytes.Buffer que escribe el servidor y lee la prueba.
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

// TestAnIssuerWithoutJWKSIsRejected: un emisor que no publica jwks_uri no sirve para verificar tokens.
func TestAnIssuerWithoutJWKSIsRejected(t *testing.T) {
	var issuer string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": issuer}) //nolint:errcheck,gosec // respuesta de prueba
	}))
	t.Cleanup(ts.Close)
	issuer = ts.URL

	_, err := newOIDCVerifier(t.Context(), issuer, testAudience, time.Now, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	if !errors.Is(err, errNoJWKS) {
		t.Fatalf("emisor sin jwks_uri = %v, se esperaba errNoJWKS", err)
	}
}
