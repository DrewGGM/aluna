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

// TokenVerifier verifica un token de acceso y devuelve su principal.
type TokenVerifier interface {
	Verify(ctx context.Context, raw string) (access.Principal, error)
}

// Límites de la verificación (ADR-0014, TH-1). Las claves del emisor valen jwksTTL desde que se pidieron:
// una clave que el IAM retira deja de valer en ese tiempo, y un IAM caído no tumba la API antes. Pasada la
// mitad se renuevan en segundo plano, así un tropiezo del IAM en la renovación no deja la API sin claves.
// Un token que las claves vigentes no verifican las vuelve a pedir, a lo sumo una vez cada jwksMinRefetch
// (una lluvia de tokens falsos no se vuelve una lluvia de descargas), o cada jwksRetry si no hay claves
// vigentes. issuerTimeout acota cada ida al emisor y maxJWKSBytes la respuesta del JWKS; maxTokenLifetime
// es la vida máxima de un token, de iat a exp, e iatSkew lo que iat puede adelantarse al reloj del core.
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
	errInvalidToken = errors.New("token inválido")
	errNoJWKS       = errors.New("el emisor OIDC no publica jwks_uri")
	errBadJWKS      = errors.New("el emisor OIDC no entregó sus claves")
	errNoPrincipal  = errors.New("request sin principal autenticado")
)

var unauthenticated = Problem{Status: http.StatusUnauthorized, Code: "unauthenticated", Title: "Falta un token de acceso válido"}

// OIDCVerifier verifica tokens JWT de un emisor OIDC (ADR-0014): firma con las claves del emisor, iss, aud,
// exp, nbf y la vida del token, y los claims sub y legal_entity_id.
type OIDCVerifier struct {
	verifier *oidc.IDTokenVerifier
	now      func() time.Time
}

// NewOIDCVerifier descubre el emisor issuer y verifica sus tokens para la audiencia audience. Las fallas al
// pedir las claves del emisor van a log: un token rechazado por eso no es culpa de quien llama.
func NewOIDCVerifier(ctx context.Context, issuer, audience string, log *slog.Logger) (*OIDCVerifier, error) {
	return newOIDCVerifier(ctx, issuer, audience, time.Now, log)
}

func newOIDCVerifier(ctx context.Context, issuer, audience string, now func() time.Time, log *slog.Logger) (*OIDCVerifier, error) {
	client := &http.Client{Timeout: issuerTimeout}
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, client), issuer)
	if err != nil {
		return nil, fmt.Errorf("descubrir el emisor OIDC %s: %w", issuer, err)
	}
	var meta struct {
		JWKSURL string `json:"jwks_uri"`
	}
	if err := provider.Claims(&meta); err != nil {
		return nil, fmt.Errorf("metadatos del emisor OIDC %s: %w", issuer, err)
	}
	if meta.JWKSURL == "" {
		return nil, fmt.Errorf("%w: %s", errNoJWKS, issuer)
	}
	keys := &issuerKeys{url: meta.JWKSURL, client: client, now: now, log: log}
	config := &oidc.Config{ClientID: audience, Now: now, SupportedSigningAlgs: []string{oidc.RS256, oidc.ES256}}
	return &OIDCVerifier{verifier: oidc.NewVerifier(issuer, keys, config), now: now}, nil
}

// Verify verifica raw y devuelve el principal de sus claims.
func (v *OIDCVerifier) Verify(ctx context.Context, raw string) (access.Principal, error) {
	token, err := v.verifier.Verify(ctx, raw)
	if err != nil {
		return access.Principal{}, fmt.Errorf("%w: %w", errInvalidToken, err)
	}
	// sin iat, IssuedAt es la fecha cero y la vida no cabe; un iat futuro estiraría la vida desde ahora
	if token.Expiry.Sub(token.IssuedAt) > maxTokenLifetime || token.IssuedAt.After(v.now().Add(iatSkew)) {
		return access.Principal{}, fmt.Errorf("%w: iat ausente o futuro, o vida de más de %s", errInvalidToken, maxTokenLifetime)
	}
	if token.Subject == "" {
		return access.Principal{}, fmt.Errorf("%w: sin sub", errInvalidToken)
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

// issuerKeys son las claves públicas de firma del emisor (ver jwksTTL).
type issuerKeys struct {
	url    string
	client *http.Client
	now    func() time.Time
	log    *slog.Logger

	// fetching ordena las descargas; mu guarda las claves y se toma solo para leerlas o cambiarlas, así una
	// descarga en curso no frena los tokens que las claves guardadas ya verifican.
	fetching sync.Mutex
	tried    time.Time // cuándo terminó el último intento de pedirlas, aunque fallara; con fetching tomado

	mu      sync.Mutex
	keys    *oidc.StaticKeySet
	fetched time.Time // cuándo se pidieron las claves guardadas
}

// VerifySignature verifica la firma de jwt con las claves vigentes y, si ninguna sirve, con las que el
// emisor publique ahora.
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

// renewInBackground renueva las claves sin hacer esperar al request, si ya toca y no hay otra descarga. El
// request toma fetching y la goroutine lo suelta al terminar: así no se lanzan dos.
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

// refresh vuelve a pedir las claves si ya toca; si no, las vigentes son las que otro request pudo acabar de
// traer.
func (k *issuerKeys) refresh(ctx context.Context) {
	k.fetching.Lock()
	defer k.fetching.Unlock()
	if k.due() {
		k.fetch(ctx)
	}
}

// due dice si pasó jwksMinRefetch desde el último intento, o jwksRetry si no hay claves vigentes. Se llama
// con fetching tomado.
func (k *issuerKeys) due() bool {
	wait := jwksMinRefetch
	if keys, _ := k.current(); len(keys.PublicKeys) == 0 {
		wait = jwksRetry
	}
	return k.now().Sub(k.tried) >= wait
}

// fetch pide las claves y, si llegan, las guarda; si no, la falla queda en log y siguen las vigentes. La
// descarga no se corta si el cliente se va: la acota issuerTimeout. El intento cuenta desde que termina:
// los requests que esperaban en fetching no repiten, uno tras otro, una descarga que se acaba de colgar. Se
// llama con fetching tomado.
func (k *issuerKeys) fetch(ctx context.Context) {
	now := k.now()
	keys, err := fetchKeys(context.WithoutCancel(ctx), k.client, k.url)
	k.tried = k.now()
	if err != nil {
		k.log.LogAttrs(ctx, slog.LevelWarn, "pedir las claves del emisor OIDC",
			slog.String("jwks_uri", k.url), slog.String("error", err.Error()))
		return
	}
	k.mu.Lock()
	k.keys, k.fetched = keys, now
	k.mu.Unlock()
}

// current son las claves vigentes, o ninguna si ya pasaron jwksTTL desde que se pidieron; stale dice que
// pasó la mitad y toca renovarlas.
func (k *issuerKeys) current() (keys *oidc.StaticKeySet, stale bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	age := k.now().Sub(k.fetched)
	if k.keys == nil || age >= jwksTTL {
		return &oidc.StaticKeySet{}, false
	}
	return k.keys, age >= jwksTTL/2
}

// fetchKeys pide el JWKS del emisor y se queda con las claves públicas RSA y EC que no son de cifrado. Una
// clave que no se puede leer (otro tipo u otra curva) se salta, como en go-oidc: no tumba las demás.
func fetchKeys(ctx context.Context, client *http.Client, url string) (*oidc.StaticKeySet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("claves del emisor OIDC: %w", err)
	}
	req.Header.Set("Cache-Control", "no-cache") // tras una rotación, un proxy no debe dar el JWKS viejo
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claves del emisor OIDC: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // cerrarlo no cambia el resultado
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", errBadJWKS, resp.StatusCode)
	}
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJWKSBytes)).Decode(&set); err != nil {
		return nil, fmt.Errorf("claves del emisor OIDC: %w", err)
	}
	if set.Keys == nil { // un 200 que no es un JWKS (un proxy, un IAM a medio arrancar) no borra las vigentes
		return nil, fmt.Errorf("%w: respuesta sin keys", errBadJWKS)
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

// Authenticate exige en cada request, salvo los probes del orquestador, un token Bearer que verifier acepte,
// y pone su principal en el contexto (FR-41: la entidad legal sale de ahí, nunca de un header). Sin token o
// con uno inválido responde 401 problem+json.
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

// bearer es el token de una cabecera Authorization "Bearer <token>" (el esquema no distingue mayúsculas).
func bearer(header string) (string, bool) {
	scheme, token, ok := strings.Cut(header, " ")
	return token, ok && strings.EqualFold(scheme, "Bearer") && token != ""
}

// principal es el principal del token del request; sin él, el handler no corre (falla cerrado).
func principal(ctx context.Context) (access.Principal, error) {
	p, ok := access.PrincipalFrom(ctx)
	if !ok {
		return access.Principal{}, errNoPrincipal
	}
	return p, nil
}
