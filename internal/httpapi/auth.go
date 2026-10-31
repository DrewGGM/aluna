package httpapi

import (
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"

	"github.com/DrewGGM/aluna/internal/access"
	"github.com/DrewGGM/aluna/internal/ids"
)

// TokenVerifier verifies an access token and returns its principal.
type TokenVerifier interface {
	Verify(ctx context.Context, raw string) (access.Principal, error)
}

// Verification limits (ADR-0014, TH-1). The issuer's keys are valid for jwksTTL from when they were fetched:
// a key the IAM retires stops being valid within that time, and an IAM that is down does not bring the API
// down sooner. Past half of it they are renewed in the background, so an IAM hiccup during the renewal does
// not leave the API without keys. A token that the current keys do not verify fetches them again, at most
// once every jwksMinRefetch (a flood of fake tokens does not become a flood of downloads), or every jwksRetry
// if there are no current keys. issuerTimeout bounds each trip to the issuer and maxJWKSBytes the JWKS
// response; maxTokenLifetime is the maximum lifetime of a token, from iat to exp, and iatSkew how far iat may
// run ahead of the core's clock.
const (
	jwksTTL          = 10 * time.Minute
	jwksMinRefetch   = 30 * time.Second
	jwksRetry        = time.Second
	issuerTimeout    = 5 * time.Second
	maxTokenLifetime = 15 * time.Minute
	iatSkew          = time.Minute
	maxJWKSBytes     = 1 << 20
)

var (
	errInvalidToken = errors.New("invalid token")
	errNoJWKS       = errors.New("the OIDC issuer does not publish jwks_uri")
	errBadJWKS      = errors.New("the OIDC issuer did not deliver its keys")
	errNoPrincipal  = errors.New("request without an authenticated principal")
)

var unauthenticated = Problem{Status: http.StatusUnauthorized, Code: "unauthenticated", Title: "A valid access token is missing"}

// OIDCVerifier verifies JWTs from an OIDC issuer (ADR-0014): the signature with the issuer's keys, iss, aud,
// exp, nbf and the token lifetime, and the sub and legal_entity_id claims.
type OIDCVerifier struct {
	verifier *oidc.IDTokenVerifier
	now      func() time.Time
}

// NewOIDCVerifier discovers the issuer issuer and verifies its tokens for the audience audience. Failures
// fetching the issuer's keys go to the log: a token rejected because of them is not the caller's fault.
func NewOIDCVerifier(ctx context.Context, issuer, audience string, log *slog.Logger) (*OIDCVerifier, error) {
	return newOIDCVerifier(ctx, issuer, audience, time.Now, log)
}

func newOIDCVerifier(ctx context.Context, issuer, audience string, now func() time.Time, log *slog.Logger) (*OIDCVerifier, error) {
	client := &http.Client{Timeout: issuerTimeout}
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, client), issuer)
	if err != nil {
		return nil, fmt.Errorf("discover OIDC issuer %s: %w", issuer, err)
	}
	var meta struct {
		JWKSURL string `json:"jwks_uri"`
	}
	if err := provider.Claims(&meta); err != nil {
		return nil, fmt.Errorf("OIDC issuer metadata %s: %w", issuer, err)
	}
	if meta.JWKSURL == "" {
		return nil, fmt.Errorf("%w: %s", errNoJWKS, issuer)
	}
	keys := &issuerKeys{url: meta.JWKSURL, client: client, now: now, log: log}
	config := &oidc.Config{ClientID: audience, Now: now, SupportedSigningAlgs: []string{oidc.RS256, oidc.ES256}}
	return &OIDCVerifier{verifier: oidc.NewVerifier(issuer, keys, config), now: now}, nil
}

// Verify verifies raw and returns the principal of its claims.
func (v *OIDCVerifier) Verify(ctx context.Context, raw string) (access.Principal, error) {
	token, err := v.verifier.Verify(ctx, raw)
	if err != nil {
		return access.Principal{}, fmt.Errorf("%w: %w", errInvalidToken, err)
	}
	// without iat, IssuedAt is the zero time and the lifetime does not fit; a future iat would stretch the
	// lifetime from now
	if token.Expiry.Sub(token.IssuedAt) > maxTokenLifetime || token.IssuedAt.After(v.now().Add(iatSkew)) {
		return access.Principal{}, fmt.Errorf("%w: missing or future iat, or lifetime over %s", errInvalidToken, maxTokenLifetime)
	}
	if token.Subject == "" {
		return access.Principal{}, fmt.Errorf("%w: no sub", errInvalidToken)
	}
	var claims struct {
		LegalEntityID string   `json:"legal_entity_id"`
		PartyID       string   `json:"party_id"`
		Roles         []string `json:"roles"`
	}
	if err := token.Claims(&claims); err != nil {
		return access.Principal{}, fmt.Errorf("%w: claims: %w", errInvalidToken, err)
	}
	entity, err := ids.Parse(claims.LegalEntityID)
	if err != nil {
		return access.Principal{}, fmt.Errorf("%w: legal_entity_id: %w", errInvalidToken, err)
	}
	p := access.Principal{Subject: token.Subject, LegalEntityID: entity, Roles: claims.Roles}
	if claims.PartyID != "" {
		party, err := ids.Parse(claims.PartyID)
		if err != nil {
			return access.Principal{}, fmt.Errorf("%w: party_id: %w", errInvalidToken, err)
		}
		p.PartyID = &party
	}
	return p, nil
}

// issuerKeys are the issuer's public signing keys (see jwksTTL).
type issuerKeys struct {
	url    string
	client *http.Client
	now    func() time.Time
	log    *slog.Logger

	// fetching serializes the downloads; mu guards the keys and is held only to read or change them, so a
	// download in progress does not hold up the tokens the stored keys already verify.
	fetching sync.Mutex
	tried    time.Time // when the last attempt to fetch them ended, even if it failed; with fetching held

	mu      sync.Mutex
	keys    *oidc.StaticKeySet
	fetched time.Time // when the stored keys were fetched
}

// VerifySignature verifies the signature of jwt with the current keys and, if none works, with those the
// issuer publishes now.
func (k *issuerKeys) VerifySignature(ctx context.Context, jwt string) ([]byte, error) {
	keys, stale := k.current()
	if payload, err := keys.VerifySignature(ctx, jwt); err == nil {
		if stale {
			k.renewInBackground(ctx)
		}
		return payload, nil
	}
	k.refresh(ctx)
	keys, _ = k.current()
	return keys.VerifySignature(ctx, jwt)
}

// renewInBackground renews the keys without making the request wait, if it is due and there is no other
// download. The request takes fetching and the goroutine releases it when done: so two are never launched.
func (k *issuerKeys) renewInBackground(ctx context.Context) {
	if !k.fetching.TryLock() {
		return
	}
	if !k.due() {
		k.fetching.Unlock()
		return
	}
	go func() {
		defer k.fetching.Unlock()
		k.fetch(ctx)
	}()
}

// refresh fetches the keys again if it is due; if not, the current ones are those another request may have
// just brought.
func (k *issuerKeys) refresh(ctx context.Context) {
	k.fetching.Lock()
	defer k.fetching.Unlock()
	if k.due() {
		k.fetch(ctx)
	}
}

// due reports whether jwksMinRefetch has passed since the last attempt, or jwksRetry if there are no current
// keys. It is called with fetching held.
func (k *issuerKeys) due() bool {
	wait := jwksMinRefetch
	if keys, _ := k.current(); len(keys.PublicKeys) == 0 {
		wait = jwksRetry
	}
	return k.now().Sub(k.tried) >= wait
}

// fetch fetches the keys and, if they arrive, stores them; if not, the failure is logged and the current ones
// stay. The download is not cut if the client leaves: issuerTimeout bounds it. The attempt counts from when
// it ends: the requests that were waiting on fetching do not repeat, one after another, a download that just
// hung. It is called with fetching held.
func (k *issuerKeys) fetch(ctx context.Context) {
	now := k.now()
	keys, err := fetchKeys(context.WithoutCancel(ctx), k.client, k.url)
	k.tried = k.now()
	if err != nil {
		k.log.LogAttrs(ctx, slog.LevelWarn, "fetch OIDC issuer keys",
			slog.String("jwks_uri", k.url), slog.String("error", err.Error()))
		return
	}
	k.mu.Lock()
	k.keys, k.fetched = keys, now
	k.mu.Unlock()
}

// current returns the current keys, or none if jwksTTL has passed since they were fetched; stale says half of
// it has passed and they are due for renewal.
func (k *issuerKeys) current() (keys *oidc.StaticKeySet, stale bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	age := k.now().Sub(k.fetched)
	if k.keys == nil || age >= jwksTTL {
		return &oidc.StaticKeySet{}, false
	}
	return k.keys, age >= jwksTTL/2
}

// fetchKeys fetches the issuer's JWKS and keeps the RSA and EC public keys that are not for encryption. A
// key that cannot be read (another type or another curve) is skipped, as in go-oidc: it does not break the
// rest.
func fetchKeys(ctx context.Context, client *http.Client, url string) (*oidc.StaticKeySet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("OIDC issuer keys: %w", err)
	}
	req.Header.Set("Cache-Control", "no-cache") // after a rotation, a proxy must not serve the old JWKS
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("OIDC issuer keys: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // closing it does not change the result
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", errBadJWKS, resp.StatusCode)
	}
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJWKSBytes)).Decode(&set); err != nil {
		return nil, fmt.Errorf("OIDC issuer keys: %w", err)
	}
	if set.Keys == nil { // a 200 that is not a JWKS (a proxy, a half-started IAM) does not wipe the current ones
		return nil, fmt.Errorf("%w: response without keys", errBadJWKS)
	}
	keys := &oidc.StaticKeySet{}
	for _, raw := range set.Keys {
		var key jose.JSONWebKey
		if json.Unmarshal(raw, &key) != nil || key.Use == "enc" {
			continue
		}
		switch key.Key.(type) {
		case *rsa.PublicKey, *ecdsa.PublicKey:
			keys.PublicKeys = append(keys.PublicKeys, key.Key)
		}
	}
	return keys, nil
}

// Authenticate requires on every request, except the orchestrator probes, a Bearer token that verifier
// accepts, and puts its principal in the context (FR-41: the legal entity comes from there, never from a
// header). Without a token or with an invalid one it answers 401 problem+json.
func Authenticate(verifier TokenVerifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == pathHealthz || r.URL.Path == pathReadyz {
				next.ServeHTTP(w, r)
				return
			}
			if raw, ok := bearer(r.Header.Get("Authorization")); ok {
				if p, err := verifier.Verify(r.Context(), raw); err == nil {
					notePrincipal(r.Context(), p)
					next.ServeHTTP(w, r.WithContext(access.WithPrincipal(r.Context(), p)))
					return
				}
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="aluna"`)
			WriteProblem(w, r, unauthenticated)
		})
	}
}

// bearer is the token of an Authorization header "Bearer <token>" (the scheme is case-insensitive).
func bearer(header string) (string, bool) {
	scheme, token, ok := strings.Cut(header, " ")
	return token, ok && strings.EqualFold(scheme, "Bearer") && token != ""
}

// principal is the principal of the request's token; without it, the handler does not run (fail closed).
func principal(ctx context.Context) (access.Principal, error) {
	p, ok := access.PrincipalFrom(ctx)
	if !ok {
		return access.Principal{}, errNoPrincipal
	}
	return p, nil
}
