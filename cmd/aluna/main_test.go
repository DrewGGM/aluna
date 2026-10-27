package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunVersion(t *testing.T) {
	var out bytes.Buffer

	code := run([]string{"version"}, &out)

	if code != 0 {
		t.Fatalf("código de salida = %d, se esperaba 0", code)
	}
	if got, want := out.String(), "aluna dev\n"; got != want {
		t.Fatalf("salida = %q, se esperaba %q", got, want)
	}
}

func TestRunVersionInyectada(t *testing.T) {
	defer func(v string) { version = v }(version)
	version = "1.2.3"
	var out bytes.Buffer

	run([]string{"version"}, &out)

	if got, want := out.String(), "aluna 1.2.3\n"; got != want {
		t.Fatalf("salida = %q, se esperaba %q", got, want)
	}
}

func TestRunSinComandoValidoImprimeUso(t *testing.T) {
	casos := map[string][]string{
		"sin argumentos":                nil,
		"comando desconocido":           {"fiar"},
		"argumentos de más":             {"version", "extra"},
		"migrate sin dirección":         {"migrate"},
		"migrate dirección inválida":    {"migrate", "sideways"},
		"migrate con argumentos de más": {"migrate", "up", "extra"},
		"api con argumentos":            {"api", "extra"},
	}
	for nombre, args := range casos {
		t.Run(nombre, func(t *testing.T) {
			defer func(w io.Writer) { stderr = w }(stderr)
			var out, errOut bytes.Buffer
			stderr = &errOut

			code := run(args, &out)

			if code != 2 {
				t.Errorf("código de salida = %d, se esperaba 2", code)
			}
			if out.Len() != 0 {
				t.Errorf("stdout = %q, se esperaba vacío", out.String())
			}
			if !strings.HasPrefix(errOut.String(), "uso: aluna") {
				t.Errorf("stderr = %q, se esperaba el uso", errOut.String())
			}
		})
	}
}

func TestServeAPIShutsDownWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	exit := make(chan int, 1)
	go func() { exit <- serveAPI(ctx, ln, quietLogger()) }()

	waitHealthy(t, ln.Addr().String())
	cancel()

	expectExit(t, exit, 0)
}

// TestRunAPIStopsOnSIGTERM prueba el camino real: la dirección sale de ALUNA_HTTP_ADDR y SIGTERM, que
// es lo que manda el orquestador en un deploy, apaga el proceso en orden.
func TestRunAPIStopsOnSIGTERM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows no permite enviar SIGTERM a un proceso; corre en CI (Linux)")
	}
	addr := freeAddr(t)
	t.Setenv("ALUNA_HTTP_ADDR", addr)
	exit := make(chan int, 1)
	go func() { exit <- run([]string{"api"}, io.Discard) }()
	waitHealthy(t, addr)

	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := self.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	expectExit(t, exit, 0)
}

func TestRunAPIInvalidAddr(t *testing.T) {
	var out bytes.Buffer

	code := runAPI(context.Background(), "127.0.0.1:-1", slog.New(slog.NewJSONHandler(&out, nil)))

	if code != 1 {
		t.Fatalf("código de salida = %d, se esperaba 1", code)
	}
	if !strings.Contains(out.String(), "no se pudo abrir el puerto") {
		t.Fatalf("log = %s", out.String())
	}
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// freeAddr reserva un puerto libre y lo suelta para que el servidor bajo prueba lo abra.
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

// waitHealthy espera hasta 5 s a que /healthz responda 200.
func waitHealthy(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			if closeErr := resp.Body.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s/healthz no respondió 200 en 5 s", addr)
}

func expectExit(t *testing.T, exit <-chan int, want int) {
	t.Helper()
	select {
	case code := <-exit:
		if code != want {
			t.Fatalf("código de salida = %d, se esperaba %d", code, want)
		}
	case <-time.After(shutdownTimeout):
		t.Fatalf("el apagado tardó más de %v", shutdownTimeout)
	}
}

func TestMigrateNeedsDatabaseURL(t *testing.T) {
	t.Setenv("ALUNA_DATABASE_URL", "")
	defer func(w io.Writer) { stderr = w }(stderr)
	var errOut bytes.Buffer
	stderr = &errOut

	code := run([]string{"migrate", "up"}, io.Discard)

	if code != 1 || !strings.Contains(errOut.String(), "ALUNA_DATABASE_URL") {
		t.Fatalf("código = %d, stderr = %q", code, errOut.String())
	}
}
