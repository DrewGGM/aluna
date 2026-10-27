package archtest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// finding es una violación del catálogo anti-slop en archivo:línea.
type finding struct {
	pos, rule string
}

var ruleText = map[string]string{
	"AS-1":  "tipo abierto (any o interface{}) en una firma o campo de domain/app",
	"AS-5":  "map[string]any como modelo de dominio",
	"AS-12": "nombre que describe la forma y no el concepto",
}

var (
	bannedPackages = []string{"utils", "common", "helpers", "shared", "misc", "base", "types"}
	bannedSuffixes = []string{"Data", "Info", "Manager", "Helper", "Util", "Impl", "Shape"}
)

// TestNoSlop recorre internal/ (este paquete vive en internal/archtest): las reglas de capa se
// reconocen por rutas como ledger/internal/domain/.
func TestNoSlop(t *testing.T) {
	for _, f := range scanSlop(t, "..") {
		t.Errorf("internal/%s: %s (%s)", f.pos, ruleText[f.rule], f.rule)
	}
}

// TestSlopFixture prueba que cada regla se dispara donde debe y no donde no.
func TestSlopFixture(t *testing.T) {
	want := []finding{
		{"ledger/internal/app/post.go:5", "AS-1"},
		{"ledger/internal/app/post.go:12", "AS-12"},
		{"ledger/internal/domain/account.go:4", "AS-12"},
		{"ledger/internal/domain/account.go:5", "AS-1"},
		{"ledger/internal/domain/account.go:6", "AS-1"},
		{"ledger/internal/domain/account.go:5", "AS-5"},
		{"ledger/internal/domain/account.go:6", "AS-5"},
		{"ledger/internal/domain/account.go:10", "AS-1"},
		{"ledger/internal/domain/account.go:13", "AS-1"},
		{"ledger/internal/domain/account.go:16", "AS-1"},
		{"utils/strings.go:1", "AS-12"},
	}

	got := scanSlop(t, filepath.Join("testdata", "slop"))

	if !slices.Equal(got, want) {
		t.Errorf("hallazgos =\n%v\nse esperaba\n%v", got, want)
	}
}

// scanSlop revisa los .go de producción no generados bajo root, sin entrar en testdata. Las pruebas
// quedan fuera: AS-1, AS-5 y AS-12 hablan de los contratos y nombres del código de producción.
func scanSlop(t *testing.T, root string) []finding {
	t.Helper()
	var found []finding
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "testdata" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		inFile, err := scanFile(path, filepath.ToSlash(rel))
		found = append(found, inFile...)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func scanFile(path, rel string) ([]finding, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parsear %s: %w", rel, err)
	}
	if ast.IsGenerated(file) {
		return nil, nil
	}
	s := &fileScan{fset: fset, rel: rel}
	if slices.Contains(bannedPackages, file.Name.Name) {
		s.report(file.Name, "AS-12")
	}
	ast.Inspect(file, s.visit)
	return s.found, nil
}

type fileScan struct {
	fset  *token.FileSet
	rel   string // relativa a la raíz recorrida, con "/"
	found []finding
}

func (s *fileScan) visit(node ast.Node) bool {
	switch n := node.(type) {
	case *ast.TypeSpec:
		s.checkTypeName(n)
	case *ast.FuncType:
		if s.inLayer("domain", "app") {
			s.checkOpenFields(n.Params)
			s.checkOpenFields(n.Results)
		}
	case *ast.StructType:
		if s.inLayer("domain", "app") {
			s.checkOpenFields(n.Fields)
		}
	case *ast.MapType:
		s.checkDictionary(n)
	}
	return true
}

func (s *fileScan) checkTypeName(spec *ast.TypeSpec) {
	if s.inLayer("domain", "app") && isOpenType(spec.Type) { // type Payload = any
		s.report(spec, "AS-1")
	}
	for _, suffix := range bannedSuffixes {
		if strings.HasSuffix(spec.Name.Name, suffix) {
			s.report(spec, "AS-12")
			return
		}
	}
}

// checkOpenFields revisa parámetros, resultados o campos; los type parameters no pasan por aquí.
func (s *fileScan) checkOpenFields(fields *ast.FieldList) {
	if fields == nil {
		return
	}
	for _, field := range fields.List {
		if containsOpenType(field.Type) {
			s.report(field, "AS-1")
		}
	}
}

func (s *fileScan) checkDictionary(m *ast.MapType) {
	key, ok := m.Key.(*ast.Ident)
	if ok && key.Name == "string" && isOpenType(m.Value) && s.inLayer("domain") {
		s.report(m, "AS-5")
	}
}

func (s *fileScan) inLayer(layers ...string) bool {
	for _, layer := range layers {
		if strings.Contains("/"+s.rel, "/internal/"+layer+"/") {
			return true
		}
	}
	return false
}

func (s *fileScan) report(node ast.Node, rule string) {
	line := s.fset.Position(node.Pos()).Line
	s.found = append(s.found, finding{fmt.Sprintf("%s:%d", s.rel, line), rule})
}

func containsOpenType(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.FuncType, *ast.StructType:
			return false // visit los revisa por su cuenta
		}
		found = found || isOpenType(n)
		return !found
	})
	return found
}

func isOpenType(n ast.Node) bool {
	switch t := n.(type) {
	case *ast.Ident:
		return t.Name == "any"
	case *ast.InterfaceType:
		return t.Methods == nil || len(t.Methods.List) == 0
	}
	return false
}
