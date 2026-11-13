package sender_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/DrewGGM/aluna/internal/webhooks/internal/adapters/sender"
)

// TestARedirectIsNotFollowed (SSRF): a redirect is the endpoint's answer, not somewhere else to send the event.
func TestARedirectIsNotFollowed(t *testing.T) {
	client := sender.New()
	if err := client.CheckRedirect(&http.Request{}, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect = %v, want ErrUseLastResponse", err)
	}
	if transport, ok := client.Transport.(*http.Transport); !ok || transport.Proxy != nil {
		t.Fatal("the client has a proxy")
	}
}
