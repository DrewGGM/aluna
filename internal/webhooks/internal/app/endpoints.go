package app

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

// ErrInvalidEndpoint means a webhook endpoint that is not an https URL of the internet: Aluna does not send requests
// into its own network (SSRF).
var ErrInvalidEndpoint = errors.New("invalid webhook endpoint")

// ErrEndpointNotFound means the entity has no webhook endpoint with that id.
var ErrEndpointNotFound = errors.New("webhook endpoint not found")

// maxURL is the longest URL an endpoint has.
const maxURL = 2048

// Resolver resolves a host's addresses.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// CheckEndpoint is nil if raw is an endpoint Aluna may send webhooks to: an https URL of up to 2048 characters, without
// credentials, whose host is or resolves only to public addresses (CheckAddress). Otherwise ErrInvalidEndpoint, also
// when the host does not resolve. The sender checks every address again when it connects, since the host may resolve
// elsewhere later.
func CheckEndpoint(ctx context.Context, r Resolver, raw string) error {
	host, err := hostOf(raw)
	if err != nil {
		return err
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return CheckAddress(addr)
	}
	addrs, err := r.LookupNetIP(ctx, "ip", strings.TrimSuffix(host, "."))
	if err != nil || len(addrs) == 0 {
		return fmt.Errorf("%w: %s does not resolve", ErrInvalidEndpoint, host)
	}
	for _, addr := range addrs {
		if err := CheckAddress(addr); err != nil {
			return err
		}
	}
	return nil
}

// hostOf is the host of raw if raw is an https URL of up to maxURL characters without credentials.
func hostOf(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || len(raw) > maxURL || u.Hostname() == "" {
		return "", fmt.Errorf("%w: an https URL of up to %d characters, without credentials", ErrInvalidEndpoint, maxURL)
	}
	return u.Hostname(), nil
}

// notPublic are the ranges that are global unicast in netip's sense but do not reach the internet, or reach an IPv4
// address of any kind through a translation: shared (carrier-grade NAT), reserved and benchmarking IPv4, and the
// IPv6 that carry an IPv4 inside (IPv4-compatible, NAT64, Teredo, 6to4) or are site-local.
var notPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("fec0::/10"),
}

// CheckAddress is nil if the sender may connect to addr: a global unicast address that is not private, loopback,
// link-local (the cloud metadata service is there), multicast, unspecified or in notPublic; ErrInvalidEndpoint
// otherwise.
func CheckAddress(addr netip.Addr) error {
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return fmt.Errorf("%w: %s is not a public address", ErrInvalidEndpoint, addr)
	}
	for _, p := range notPublic {
		if p.Contains(addr) {
			return fmt.Errorf("%w: %s is not a public address", ErrInvalidEndpoint, addr)
		}
	}
	return nil
}
