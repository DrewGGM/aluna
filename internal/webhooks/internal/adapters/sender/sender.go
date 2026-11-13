// Package sender is the HTTP client webhooks are sent with: it only connects to public addresses, so an endpoint
// whose name later resolves into Aluna's network does not reach it (SSRF), and it does not follow redirects.
package sender

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"

	"github.com/DrewGGM/aluna/internal/webhooks/internal/app"
)

// timeout bounds an attempt: connecting, sending the event and getting the answer.
const timeout = 15 * time.Second

// New is the client webhooks are sent with. It has no proxy, not even the environment's: a proxy would connect for it
// to an address the dialer did not check.
func New() *http.Client {
	dialer := &net.Dialer{Timeout: timeout, Control: publicOnly}
	transport := &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: timeout, IdleConnTimeout: 90 * time.Second}
	return &http.Client{
		Timeout: timeout, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// publicOnly refuses to connect to an address that is not public, after the name was resolved.
func publicOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("webhook address %q: %w", address, err)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("webhook address %q: %w", address, err)
	}
	return app.CheckAddress(addr)
}
