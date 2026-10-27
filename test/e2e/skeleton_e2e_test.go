//go:build e2e

// Package e2e prueba el binario aluna de punta a punta: proceso real, PostgreSQL real y HTTP.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// TestSkeleton: una transacción balanceada de 100.000 COP viaja HTTP → ledger → PostgreSQL → HTTP
// con el binario compilado, migrado por él mismo y conectado con el rol de la aplicación.
func TestSkeleton(t *testing.T) {
	bin := buildAluna(t)
	dsn, cleanup, err := pgtest.StartEmpty(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	if out, err := alunaCmd(t, bin, []string{"ALUNA_DATABASE_URL=" + dsn}, "migrate", "up").CombinedOutput(); err != nil {
		t.Fatalf("aluna migrate up: %v\n%s", err, out)
	}
	base := startAPI(t, bin, dsn)

	cash, deposit := createAccount(t, base, "asset"), createAccount(t, base, "liability")
	posting := fmt.Sprintf(`{"code":"deposito","value_date":"2026-10-26","business_date":"2026-10-26",
		"source":"e2e","external_ref":%q,"entries":[
		{"account_id":%q,"amount":{"amount":10000000,"currency":"COP"}},
		{"account_id":%q,"amount":{"amount":-10000000,"currency":"COP"}}]}`, ids.New().String(), cash, deposit)
	if status := call(t, http.MethodPost, base+"/v1/transactions", posting, nil); status != http.StatusCreated {
		t.Fatalf("asentar = %d", status)
	}

	for account, posted := range map[string]int64{cash: 10_000_000, deposit: -10_000_000} {
		var b struct{ Posted, Natural struct{ Amount int64 } }
		if status := call(t, http.MethodGet, base+"/v1/ledger-accounts/"+account+"/balance", "", &b); status != http.StatusOK {
			t.Fatalf("saldo de %s = %d", account, status)
		}
		if b.Posted.Amount != posted || b.Natural.Amount != 10_000_000 {
			t.Errorf("saldo de %s = %+v, se esperaba con signo %d y natural 10.000.000 COP", account, b, posted)
		}
	}
}

// buildAluna compila cmd/aluna en un directorio temporal.
func buildAluna(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "aluna")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.CommandContext(t.Context(), "go", "build", "-race", "-o", bin, "github.com/DrewGGM/aluna/cmd/aluna").CombinedOutput(); err != nil { //nolint:gosec // bin es una ruta del TempDir de la prueba
		t.Fatalf("compilar aluna: %v\n%s", err, out)
	}
	return bin
}

// alunaCmd prepara el binario con el entorno del proceso más env; sin exportar telemetría.
func alunaCmd(t *testing.T, bin string, env []string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), bin, args...) //nolint:gosec // bin lo compiló buildAluna
	cmd.Env = append(os.Environ(), append([]string{"OTEL_EXPORTER_OTLP_ENDPOINT="}, env...)...)
	return cmd
}

// startAPI arranca aluna api con el rol aluna_app, espera /readyz y devuelve su URL. El proceso muere
// al terminar la prueba; si falla, su log sale en la prueba.
func startAPI(t *testing.T, bin, dsn string) string {
	t.Helper()
	appDSN, err := pgtest.AppDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	addr := freeAddr(t)
	cmd := alunaCmd(t, bin, []string{
		"ALUNA_HTTP_ADDR=" + addr, "ALUNA_DATABASE_URL=" + appDSN, "ALUNA_LEGAL_ENTITY_ID=" + ids.New().String(),
	}, "api")
	var logs bytes.Buffer // os/exec no escribe a la vez en Stdout y Stderr si son el mismo writer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Wait() //nolint:errcheck,gosec // el proceso muere con el contexto de la prueba
		if t.Failed() {
			t.Logf("log de aluna api:\n%s", logs.String())
		}
	})
	base := "http://" + addr
	waitReady(t, base+"/readyz")
	return base
}

// waitReady sondea url hasta que responde 200; mientras el proceso arranca, la conexión falla.
func waitReady(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close() //nolint:errcheck,gosec // cierre en prueba
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s no respondió 200 en 30 s", url)
}

func createAccount(t *testing.T, base, kind string) string {
	t.Helper()
	var account struct{ ID string }
	body := fmt.Sprintf(`{"code":"e2e-%s","kind":%q,"currency":"COP","balance_mode":"sync"}`, ids.New(), kind)
	if status := call(t, http.MethodPost, base+"/v1/ledger-accounts", body, &account); status != http.StatusCreated {
		t.Fatalf("crear cuenta %s = %d", kind, status)
	}
	return account.ID
}

// client no deja que un proceso colgado cuelgue la prueba.
var client = &http.Client{Timeout: 10 * time.Second}

// call hace un request JSON y decodifica la respuesta en out si no es nil.
func call(t *testing.T, method, url, body string, out any) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", ids.New().String())
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close() //nolint:errcheck // cierre en prueba
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s = %d: cuerpo inválido: %v", method, url, resp.StatusCode, err)
		}
	}
	return resp.StatusCode
}

// freeAddr reserva un puerto libre y lo suelta para que lo tome el proceso.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}
