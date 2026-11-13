package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// healthzServer serves /healthz with status on the loopback and returns its port.
func healthzServer(t *testing.T, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestHealthcheckPassesWhenTheAPIIsAlive(t *testing.T) {
	port := healthzServer(t, http.StatusOK)
	for _, addr := range []string{":" + port, "0.0.0.0:" + port, "127.0.0.1:" + port} {
		if err := healthcheck(t.Context(), addr); err != nil {
			t.Errorf("healthcheck(%q) = %v, want nil", addr, err)
		}
	}
}

func TestHealthcheckFails(t *testing.T) {
	cases := map[string]string{
		"unhealthy answer": ":" + healthzServer(t, http.StatusServiceUnavailable),
		"nobody listening": "127.0.0.1:1",
		"invalid address":  "8080",
	}
	for name, addr := range cases {
		t.Run(name, func(t *testing.T) {
			if err := healthcheck(t.Context(), addr); err == nil {
				t.Errorf("healthcheck(%q) = nil, want an error", addr)
			}
		})
	}
}

func TestRunHealthcheckExitCode(t *testing.T) {
	t.Setenv("ALUNA_HTTP_ADDR", ":"+healthzServer(t, http.StatusOK))
	if code := run([]string{"healthcheck"}, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
}
