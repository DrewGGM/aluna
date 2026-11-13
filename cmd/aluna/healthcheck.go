package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// healthcheckTimeout bounds one probe: the container runtime runs it on every interval.
const healthcheckTimeout = 3 * time.Second

// probeClient goes straight to the api, never through an HTTP(S)_PROXY of the environment.
var probeClient = &http.Client{Transport: &http.Transport{Proxy: nil}}

// healthcheck asks the api listening on addr for /healthz from inside its own container, whose image has no
// shell or curl. An unspecified host (":8080", "0.0.0.0:8080") is probed on the loopback.
func healthcheck(ctx context.Context, addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("healthcheck address %q: %w", addr, err)
	}
	if ip := net.ParseIP(host); host == "" || ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/healthz", http.NoBody)
	if err != nil {
		return err
	}
	res, err := probeClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close() //nolint:errcheck // only the status matters
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz answered %s", res.Status)
	}
	return nil
}
