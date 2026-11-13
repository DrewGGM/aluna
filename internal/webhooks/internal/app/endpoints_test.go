package app_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/DrewGGM/aluna/internal/webhooks/internal/app"
)

// dns resolves each host to its addresses; any other does not resolve.
type dns map[string][]string

func (d dns) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	addrs := make([]netip.Addr, 0, len(d[host]))
	for _, a := range d[host] {
		addrs = append(addrs, netip.MustParseAddr(a))
	}
	if len(addrs) == 0 {
		return nil, errors.New("no such host")
	}
	return addrs, nil
}

// TestAnEndpointIsOfTheInternet (FR-46, SSRF): https to public addresses only, by literal or by name, every one of a
// name's.
func TestAnEndpointIsOfTheInternet(t *testing.T) {
	resolver := dns{
		"hooks.example.com":    {"93.184.216.34", "2606:2800:220:1::1"},
		"internal.example.com": {"93.184.216.34", "10.0.0.7"},
		"metadata.example.com": {"169.254.169.254"},
	}
	for raw, ok := range map[string]bool{
		"https://hooks.example.com/aluna":           true,
		"https://hooks.example.com.:8443/aluna?x=1": true,
		"https://93.184.216.34/aluna":               true,
		"http://hooks.example.com/aluna":            false,
		"https://user:pass@hooks.example.com/":      false,
		"https://internal.example.com/":             false,
		"https://metadata.example.com/":             false,
		"https://nowhere.example.com/":              false,
		"https://127.0.0.1/":                        false,
		"https://[::1]/":                            false,
		"https://10.1.2.3/":                         false,
		"https://192.168.0.1/":                      false,
		"https://172.16.0.1/":                       false,
		"https://169.254.169.254/latest/meta-data/": false,
		"https://100.64.0.1/":                       false,
		"https://0.0.0.0/":                          false,
		"https://[fc00::1]/":                        false,
		"https://[fe80::1]/":                        false,
		"https://[::ffff:127.0.0.1]/":               false,
		"https://[::ffff:100.64.0.1]/":              false,
		"https://224.0.0.1/":                        false,
		"https://198.18.0.1/":                       false,
		"https://192.0.0.8/":                        false,
		"https://240.0.0.1/":                        false,
		"https://[64:ff9b::a00:1]/":                 false,
		"https://[64:ff9b::a9fe:a9fe]/":             false,
		"https://[64:ff9b:1::a00:1]/":               false,
		"https://[2002:a00:1::1]/":                  false,
		"https://[2001::a00:1]/":                    false,
		"https://[::7f00:1]/":                       false,
		"https://[fec0::1]/":                        false,
		"https:///nohost":                           false,
		"not a url":                                 false,
	} {
		err := app.CheckEndpoint(t.Context(), resolver, raw)
		if ok != (err == nil) || (!ok && !errors.Is(err, app.ErrInvalidEndpoint)) {
			t.Errorf("%s: %v; want ok %v", raw, err, ok)
		}
	}
}
