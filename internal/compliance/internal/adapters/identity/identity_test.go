package identity

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/DrewGGM/aluna/internal/compliance/internal/app"
	"github.com/DrewGGM/aluna/internal/ids"
)

// entity is the legal entity of the subjects the service knows.
var entity = ids.New()

// service answers, for the token "t0k3n", entity's party onboarding-1 and key @llave/con espacio, something else
// than an identity for onboarding-2, a redirect for onboarding-3 and nothing for anyone else.
func service(t *testing.T) *Service {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch at := "/identity/" + entity.String(); {
		case r.Header.Get("Authorization") != "Bearer t0k3n":
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.EscapedPath() == at+"/party/onboarding-1":
			_, _ = w.Write([]byte(`{"names": ["Ana María Pérez"], "documents": ["52111222"]}`))
		case r.URL.EscapedPath() == at+"/key/@llave%2Fcon%20espacio":
			_, _ = w.Write([]byte(`{"names": ["Luis Gómez"]}`))
		case r.URL.Path == at+"/party/onboarding-2":
			_, _ = w.Write([]byte(`<html>not an identity</html>`))
		case r.URL.Path == at+"/party/onboarding-3":
			http.Redirect(w, r, at+"/party/onboarding-1", http.StatusFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)
	s, err := New(ts.URL+"/identity/", "t0k3n", ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// subject is entity's subject of kind with ref.
func subject(kind app.SubjectKind, ref string) app.Subject {
	return app.Subject{Kind: kind, ID: ids.New(), LegalEntityID: entity, ExternalRef: ref}
}

// TestASubjectIsWhoTheEntitySays (ADR-0018): the identity service answers a party's names and documents, and a
// beneficiary's by its key, escaped as one segment, its slash too, among those of the subject's entity.
func TestASubjectIsWhoTheEntitySays(t *testing.T) {
	s := service(t)
	who, err := s.Identify(t.Context(), subject(app.SubjectParty, "onboarding-1"))
	if err != nil || !slices.Equal(who.Names, []string{"Ana María Pérez"}) || !slices.Equal(who.Documents, []string{"52111222"}) {
		t.Fatalf("a party = %+v (%v)", who, err)
	}
	if who, err := s.Identify(t.Context(), subject(app.SubjectKey, "@llave/con espacio")); err != nil || who.Names[0] != "Luis Gómez" {
		t.Fatalf("a beneficiary by key = %+v (%v)", who, err)
	}
}

// TestAnUnknownSubjectOrAnUnusableAnswerIsNoIdentity (ADR-0018): a subject the service does not know, of another
// entity, an account it does not know though a key is written the same, or a reference that is no segment, is
// ErrUnknownSubject; an answer that is no identity, a redirect, or the service refusing the token, an error: none of
// them clears anyone.
func TestAnUnknownSubjectOrAnUnusableAnswerIsNoIdentity(t *testing.T) {
	s := service(t)
	another := subject(app.SubjectParty, "onboarding-1")
	another.LegalEntityID = ids.New()
	for name, unknown := range map[string]app.Subject{
		"an unknown subject": subject(app.SubjectParty, "nobody"), "another entity's": another,
		"an account written as a key": subject(app.SubjectAccount, "@llave/con espacio"), "no segment": subject(app.SubjectParty, ".."),
	} {
		if _, err := s.Identify(t.Context(), unknown); !errors.Is(err, app.ErrUnknownSubject) {
			t.Fatalf("%s = %v, want ErrUnknownSubject", name, err)
		}
	}
	for name, ref := range map[string]string{"an answer that is no identity": "onboarding-2", "a redirect": "onboarding-3"} {
		if _, err := s.Identify(t.Context(), subject(app.SubjectParty, ref)); !errors.Is(err, errService) {
			t.Fatalf("%s = %v, want errService", name, err)
		}
	}
	s.token = "wrong"
	if _, err := s.Identify(t.Context(), subject(app.SubjectParty, "onboarding-1")); !errors.Is(err, errService) {
		t.Fatalf("a refused token = %v, want errService", err)
	}
}

// TestAnIdentityServiceNeedsItsURLAndToken: a URL that is not https, or http to another host, carries credentials
// or a query, or no token, is refused.
func TestAnIdentityServiceNeedsItsURLAndToken(t *testing.T) {
	for name, tc := range map[string]struct{ url, token string }{
		"another scheme": {"ftp://host/identity", "t"},
		"http elsewhere": {"http://onboarding.example/identity", "t"},
		"credentials":    {"https://u:p@host/identity", "t"},
		"a query":        {"https://host/identity?x=1", "t"},
		"no token":       {"https://host/identity", ""},
	} {
		if _, err := New(tc.url, tc.token, &http.Client{}); !errors.Is(err, errService) {
			t.Errorf("%s = %v, want errService", name, err)
		}
	}
}
