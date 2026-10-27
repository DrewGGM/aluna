package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
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
		"sin argumentos":      nil,
		"comando desconocido": {"fiar"},
		"argumentos de más":   {"version", "extra"},
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
