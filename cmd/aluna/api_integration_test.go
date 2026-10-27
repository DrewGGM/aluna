//go:build integration

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"syscall"
	"testing"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

var testDSN string

func TestMain(m *testing.M) {
	dsn, cleanup, err := pgtest.Start(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testDSN = dsn
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// TestRunAPIStopsOnSIGTERM prueba el camino real: la configuración sale del entorno, la API queda lista
// contra la base y SIGTERM, lo que manda el orquestador en un deploy, la apaga en orden.
func TestRunAPIStopsOnSIGTERM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows no permite enviar SIGTERM a un proceso; corre en CI (Linux)")
	}
	addr := freeAddr(t)
	t.Setenv("ALUNA_HTTP_ADDR", addr)
	appDSN, err := pgtest.AppDSN(testDSN) // como en producción: el rol de la aplicación, no el dueño
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ALUNA_DATABASE_URL", appDSN)
	t.Setenv("ALUNA_LEGAL_ENTITY_ID", ids.New().String())
	exit := make(chan int, 1)
	go func() { exit <- run([]string{"api"}, io.Discard) }()
	waitHealthy(t, addr)
	resp, err := http.Get("http://" + addr + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("/readyz = %d (%v): con la base arriba la api está lista", resp.StatusCode, err)
	}

	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := self.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	expectExit(t, exit, 0)
}

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
