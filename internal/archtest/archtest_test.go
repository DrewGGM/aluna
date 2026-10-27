package archtest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const internalPrefix = modulePath + "/internal/"

// goPackage es la parte de la salida de `go list -json` que usa la prueba. Solo se leen los imports
// de producción: una prueba puede montar varios módulos para preparar un escenario.
type goPackage struct {
	ImportPath string
	Imports    []string
}

type violation struct {
	from, to, reason string
}

func TestArchitecture(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-json", internalPrefix+"...").Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		t.Fatalf("go list: %v\n%s", err, exitErr.Stderr)
	}
	if err != nil {
		t.Fatalf("go list: %v", err)
	}

	for _, v := range checkArchitecture(decodePackages(t, out)) {
		t.Errorf("%s → %s: %s", v.from, v.to, v.reason)
	}
}

// TestArchViolationFixture prueba con una salida de `go list` simulada que cada import prohibido se
// rechaza y que los permitidos pasan.
func TestArchViolationFixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "violations.json"))
	if err != nil {
		t.Fatal(err)
	}
	const in = internalPrefix
	want := []violation{
		{in + "ledger", in + "deposits", "arista fuera del DAG"},
		{in + "ledger", in + "loans", "importa un paquete no declarado en dag_test.go"},
		{in + "deposits/internal/app", in + "httpapi", "nadie importa un adaptador de entrada"},
		{in + "money", in + "ledger", "un paquete base no importa módulos"},
		{in + "jobs", in + "httpapi", "nadie importa un adaptador de entrada"},
		{in + "loans", "", "paquete no declarado en dag_test.go"},
		{in + "ledger/internal/domain", "github.com/jackc/pgx/v5", domainRule},
		{in + "ledger/internal/domain", in + "postgres", domainRule},
	}

	got := checkArchitecture(decodePackages(t, fixture))

	if !slices.Equal(got, want) {
		t.Errorf("violaciones =\n%v\nse esperaba\n%v", got, want)
	}
}

func TestDAGIsAcyclic(t *testing.T) {
	const (
		visiting = 1
		done     = 2
	)
	state := map[string]int{}
	var visit func(module string, path []string)
	visit = func(module string, path []string) {
		switch state[module] {
		case visiting:
			t.Errorf("ciclo: %s", strings.Join(append(path, module), " → "))
			return
		case done:
			return
		}
		state[module] = visiting
		for _, dep := range moduleDeps[module] {
			if _, ok := moduleDeps[dep]; !ok {
				t.Errorf("%s depende de %s, que no es un módulo declarado", module, dep)
			}
			visit(dep, append(path, module))
		}
		state[module] = done
	}
	for _, module := range slices.Sorted(maps.Keys(moduleDeps)) {
		visit(module, nil)
	}
}

func checkArchitecture(pkgs []goPackage) []violation {
	var found []violation
	for _, p := range pkgs {
		owner, ok := internalOwner(p.ImportPath)
		if !ok || owner == "archtest" { // archtest solo tiene pruebas y no se declara.
			continue
		}
		if !declared(owner) {
			found = append(found, violation{p.ImportPath, "", "paquete no declarado en dag_test.go"})
			continue
		}
		found = append(found, checkImports(p, owner)...)
	}
	return found
}

func checkImports(p goPackage, owner string) []violation {
	var found []violation
	inDomain := isDomain(p.ImportPath)
	for _, imp := range p.Imports {
		if inDomain && !domainMayImport(owner, imp) {
			found = append(found, violation{p.ImportPath, imp, domainRule})
			continue
		}
		to, ok := internalOwner(imp)
		if !ok || to == owner {
			continue
		}
		if reason := edgeProblem(owner, to); reason != "" {
			found = append(found, violation{p.ImportPath, imp, reason})
		}
	}
	return found
}

// internalOwner devuelve el paquete de primer nivel bajo internal/ al que pertenece importPath.
func internalOwner(importPath string) (string, bool) {
	rest, ok := strings.CutPrefix(importPath, internalPrefix)
	if !ok {
		return "", false
	}
	owner, _, _ := strings.Cut(rest, "/")
	return owner, true
}

// edgeProblem dice por qué el paquete declarado from no puede importar a to, o "" si puede.
func edgeProblem(from, to string) string {
	switch {
	case !declared(to):
		return "importa un paquete no declarado en dag_test.go"
	case slices.Contains(entrypoints, to):
		return "nadie importa un adaptador de entrada"
	case slices.Contains(foundation, to), slices.Contains(entrypoints, from):
		return ""
	case slices.Contains(foundation, from):
		return "un paquete base no importa módulos"
	case !slices.Contains(moduleDeps[from], to):
		return "arista fuera del DAG"
	}
	return ""
}

const domainRule = "el dominio solo importa stdlib, money, ids, clock, su propio domain y fachadas del DAG"

// domainMayImport aplica la enmienda de ADR-0001 a un import de producción de un paquete domain.
func domainMayImport(owner, imp string) bool {
	first, _, _ := strings.Cut(imp, "/")
	if !strings.Contains(first, ".") { // stdlib
		return true
	}
	if o, _ := internalOwner(imp); o == owner && isDomain(imp) {
		return true
	}
	to, ok := internalOwner(imp)
	if !ok || imp != internalPrefix+to { // solo la fachada raíz, nunca sus internals
		return false
	}
	return slices.Contains(kernel, to) || slices.Contains(moduleDeps[owner], to)
}

// isDomain dice si importPath es un paquete domain o uno de sus subpaquetes.
func isDomain(importPath string) bool {
	return strings.HasSuffix(importPath, "/internal/domain") || strings.Contains(importPath, "/internal/domain/")
}

func declared(pkg string) bool {
	_, isModule := moduleDeps[pkg]
	return isModule || slices.Contains(foundation, pkg) || slices.Contains(entrypoints, pkg)
}

func decodePackages(t *testing.T, data []byte) []goPackage {
	t.Helper()
	var pkgs []goPackage
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var p goPackage
		err := dec.Decode(&p)
		if errors.Is(err, io.EOF) {
			return pkgs
		}
		if err != nil {
			t.Fatalf("salida de go list: %v", err)
		}
		pkgs = append(pkgs, p)
	}
}
