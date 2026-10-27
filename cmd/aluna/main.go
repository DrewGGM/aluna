// Command aluna es el binario único del core bancario: cada proceso (API, worker, migraciones)
// es un subcomando.
package main

import (
	"fmt"
	"io"
	"os"
)

// version se inyecta al compilar: go build -ldflags "-X main.version=1.2.3" ./cmd/aluna
var version = "dev"

// stderr es una variable para que las pruebas capturen el uso.
var stderr io.Writer = os.Stderr

const usage = `uso: aluna <comando>

comandos:
  version   imprime la versión
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout))
}

// run ejecuta el subcomando indicado y devuelve el código de salida del proceso.
func run(args []string, stdout io.Writer) int {
	if len(args) != 1 {
		return printUsage()
	}
	switch args[0] {
	case "version":
		if _, err := fmt.Fprintf(stdout, "aluna %s\n", version); err != nil {
			return 1
		}
		return 0
	default:
		return printUsage()
	}
}

func printUsage() int {
	fmt.Fprint(stderr, usage) //nolint:errcheck // si stderr falla no hay dónde reportarlo
	return 2
}
