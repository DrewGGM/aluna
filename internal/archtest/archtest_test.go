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

// goPackage is the part of the `go list -json` output the test uses. Only production imports are read:
// a test may wire several modules together to set up a scenario.
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

// TestArchViolationFixture checks, against a simulated `go list` output, that every forbidden import is
// rejected and the allowed ones pass.
func TestArchViolationFixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "violations.json"))
	if err != nil {
		t.Fatal(err)
	}
	const in = internalPrefix
	want := []violation{
		{in + "ledger", in + "deposits", "edge outside the DAG"},
		{in + "ledger", in + "loans", "imports a package not declared in dag_test.go"},
		{in + "deposits/internal/app", in + "httpapi", "nobody imports an inbound adapter"},
		{in + "money", in + "ledger", "a foundation package does not import modules"},
		{in + "jobs", in + "httpapi", "nobody imports an inbound adapter"},
		{in + "loans", "", "package not declared in dag_test.go"},
		{in + "ledger/internal/domain", "github.com/jackc/pgx/v5", domainRule},
		{in + "ledger/internal/domain", in + "postgres", domainRule},
	}

	got := checkArchitecture(decodePackages(t, fixture))

	if !slices.Equal(got, want) {
		t.Errorf("violations =\n%v\nwant\n%v", got, want)
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
			t.Errorf("cycle: %s", strings.Join(append(path, module), " → "))
			return
		case done:
			return
		}
		state[module] = visiting
		for _, dep := range moduleDeps[module] {
			if _, ok := moduleDeps[dep]; !ok {
				t.Errorf("%s depends on %s, which is not a declared module", module, dep)
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
		if !ok || owner == "archtest" { // archtest only has tests and is not declared.
			continue
		}
		if !declared(owner) {
			found = append(found, violation{p.ImportPath, "", "package not declared in dag_test.go"})
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

// internalOwner returns the top-level package under internal/ that importPath belongs to.
func internalOwner(importPath string) (string, bool) {
	rest, ok := strings.CutPrefix(importPath, internalPrefix)
	if !ok {
		return "", false
	}
	owner, _, _ := strings.Cut(rest, "/")
	return owner, true
}

// edgeProblem says why the declared package from cannot import to, or "" if it can.
func edgeProblem(from, to string) string {
	switch {
	case !declared(to):
		return "imports a package not declared in dag_test.go"
	case slices.Contains(entrypoints, to):
		return "nobody imports an inbound adapter"
	case slices.Contains(foundation, to), slices.Contains(entrypoints, from):
		return ""
	case slices.Contains(foundation, from):
		return "a foundation package does not import modules"
	case !slices.Contains(moduleDeps[from], to):
		return "edge outside the DAG"
	}
	return ""
}

const domainRule = "the domain only imports stdlib, money, ids, clock, its own domain and DAG facades"

// domainMayImport applies the ADR-0001 amendment to a production import of a domain package.
func domainMayImport(owner, imp string) bool {
	first, _, _ := strings.Cut(imp, "/")
	if !strings.Contains(first, ".") { // stdlib
		return true
	}
	if o, _ := internalOwner(imp); o == owner && isDomain(imp) {
		return true
	}
	to, ok := internalOwner(imp)
	if !ok || imp != internalPrefix+to { // only the root facade, never its internals
		return false
	}
	return slices.Contains(kernel, to) || slices.Contains(moduleDeps[owner], to)
}

// isDomain reports whether importPath is a domain package or one of its subpackages.
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
			t.Fatalf("go list output: %v", err)
		}
		pkgs = append(pkgs, p)
	}
}
