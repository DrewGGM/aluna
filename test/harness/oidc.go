//go:build integration || e2e || load || restore

package harness

import (
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/coreos/go-oidc/v3/oidc/oidctest"

	"github.com/DrewGGM/aluna/internal/ids"
)

const (
	// audience is the audience the issuer's tokens are for; StartAPI starts the api with it.
	audience = "aluna-api"
	// keyID names the issuer's only signing key in its JWKS and in the tokens' header.
	keyID = "k1"
)

// Issuer is a test OIDC issuer that the api discovers at URL and whose tokens it accepts.
type Issuer struct {
	URL string
	key *rsa.PrivateKey
}

// NewIssuer starts an issuer that stops when the test ends.
func NewIssuer(t testing.TB) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := &oidctest.Server{PublicKeys: []oidctest.PublicKey{{PublicKey: key.Public(), KeyID: keyID, Algorithm: oidc.RS256}}}
	ts := httptest.NewUnstartedServer(s)
	// before it serves: afterwards requests would read the issuer from another goroutine while it is written
	s.SetIssuer("http://" + ts.Listener.Addr().String())
	ts.Start()
	t.Cleanup(ts.Close)
	return &Issuer{URL: ts.URL, key: key}
}

// Sign signs, with the 15-minute maximum lifetime, the token of subject with role in entity. It may run outside the
// test's goroutine.
func (i *Issuer) Sign(subject string, entity ids.ID, role string) string {
	now := time.Now()
	// %q quotes these ASCII values as JSON does, and leaves no marshaling error to report from another goroutine
	claims := fmt.Sprintf(`{"iss":%q,"aud":%q,"sub":%q,"iat":%d,"exp":%d,"legal_entity_id":%q,"roles":[%q]}`,
		i.URL, audience, subject, now.Unix(), now.Add(15*time.Minute).Unix(), entity, role)
	return oidctest.SignIDToken(i.key, keyID, oidc.RS256, claims)
}
