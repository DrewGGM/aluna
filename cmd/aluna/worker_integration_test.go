//go:build integration

package main

import (
	"io"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// TestRunWorkerStopsOnSIGTERM: aluna worker se conecta como aluna_app, gana la elección de líder y, con
// SIGTERM, se apaga en orden y suelta el liderazgo para que otro worker lo tome.
func TestRunWorkerStopsOnSIGTERM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows no permite enviar SIGTERM a un proceso; corre en CI (Linux)")
	}
	appDSN, err := pgtest.AppDSN(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ALUNA_DATABASE_URL", appDSN)
	exit := make(chan int, 1)
	go func() { exit <- run([]string{"worker"}, io.Discard) }()
	waitLeaders(t, 1)

	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := self.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	expectExit(t, exit, 0)
	waitLeaders(t, 0)
}

// waitLeaders espera hasta 10 s a que infra.river_leader tenga want filas.
func waitLeaders(t *testing.T, want int) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context()) //nolint:errcheck // la prueba ya terminó con la conexión
	deadline := time.Now().Add(10 * time.Second)
	for {
		var leaders int
		if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM infra.river_leader`).Scan(&leaders); err != nil {
			t.Fatal(err)
		}
		if leaders == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("infra.river_leader tiene %d filas, se esperaban %d", leaders, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
