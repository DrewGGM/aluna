// Package identity asks the entity's identity service who a screened subject is (ADR-0018): Aluna keeps no names
// or documents, its onboarding system (for a party) or the rail's directory (for a beneficiary's key or account)
// does. The contract is GET {base}/{legal entity id}/{party|key|account}/{external reference}, with a bearer
// token, answering 200 with {"names": [...], "documents": [...]}, or 404 if it does not know the subject.
package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/DrewGGM/aluna/internal/compliance/internal/app"
)

// maxIdentity bounds what is read of an answer: some names and documents.
const maxIdentity = 64 << 10

// errService means an identity service that cannot be used, or that answered something else than an identity.
var errService = errors.New("unusable identity service")

// Service is the identity service at base, through client, with token.
type Service struct {
	base   *url.URL
	token  string
	client *http.Client
}

// New is the identity service at baseURL, authorized by token, through client: https://, or http:// only to this
// host (a sidecar), since the token goes with every request, which follows no redirect for the same reason.
func New(baseURL, token string, client *http.Client) (*Service, error) {
	base, err := url.Parse(strings.TrimSuffix(baseURL, "/"))
	switch {
	case err != nil:
		return nil, fmt.Errorf("%w: its URL does not parse", errService)
	case base.Scheme != "https" && (base.Scheme != "http" || !loopback(base.Hostname())), base.User != nil, base.RawQuery != "":
		return nil, fmt.Errorf("%w: its URL must be https (http only to this host), without credentials or query", errService)
	case token == "":
		return nil, fmt.Errorf("%w: it needs its token", errService)
	}
	noRedirects := *client
	noRedirects.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Service{base: base, token: token, client: &noRedirects}, nil
}

// loopback reports whether host is this host.
func loopback(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || ip != nil && ip.IsLoopback()
}

// Identify is who s is, as the service answers it: app.ErrUnknownSubject if it does not know it, or the reference
// is no segment of a path ("." or ".."); an error if it does not answer an identity. Its errors name the subject by
// its id, never by its reference, a beneficiary's key or account.
func (s *Service) Identify(ctx context.Context, subject app.Subject) (app.Identity, error) {
	if subject.ExternalRef == "" || subject.ExternalRef == "." || subject.ExternalRef == ".." {
		return app.Identity{}, fmt.Errorf("%w: %s", app.ErrUnknownSubject, subject.ID)
	}
	target := *s.base // the reference is one segment, its slashes escaped too
	under := "/" + subject.LegalEntityID.String() + "/" + string(subject.Kind) + "/"
	target.RawPath = s.base.EscapedPath() + under + url.PathEscape(subject.ExternalRef)
	target.Path += under + subject.ExternalRef
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), http.NoBody)
	if err != nil {
		return app.Identity{}, err
	}
	r.Header.Set("Authorization", "Bearer "+s.token)
	r.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(r)
	if urlErr := (*url.Error)(nil); errors.As(err, &urlErr) {
		err = urlErr.Err // without the URL, which carries the reference
	}
	if err != nil {
		return app.Identity{}, fmt.Errorf("identify subject %s: %w", subject.ID, err)
	}
	defer resp.Body.Close() //nolint:errcheck // the answer is read whole; closing it cannot lose anything
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return app.Identity{}, fmt.Errorf("%w: %s", app.ErrUnknownSubject, subject.ID)
	default:
		return app.Identity{}, fmt.Errorf("identify subject %s: %w: it answered %d", subject.ID, errService, resp.StatusCode)
	}
	var who struct {
		Names     []string `json:"names"`
		Documents []string `json:"documents"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxIdentity)).Decode(&who); err != nil {
		return app.Identity{}, fmt.Errorf("identify subject %s: %w: its answer is no identity", subject.ID, errService)
	}
	return app.Identity{Names: who.Names, Documents: who.Documents}, nil
}
