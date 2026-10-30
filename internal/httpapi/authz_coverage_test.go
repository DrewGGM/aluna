package httpapi

import (
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	contract "github.com/DrewGGM/aluna/api"
	"github.com/DrewGGM/aluna/config"
	"github.com/DrewGGM/aluna/internal/access"
	"github.com/DrewGGM/aluna/internal/ids"
)

// testAuthorizer es la autorización de producción: la política y el contrato que viajan en el binario.
func testAuthorizer(t *testing.T) *Authorizer {
	t.Helper()
	policy, err := access.ParsePolicy(config.Roles)
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewAuthorizer(contract.OpenAPI, policy)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// notOperations son las claves de una ruta del contrato que no son operaciones (OpenAPI 3.1, Path Item).
var notOperations = []string{"$ref", "summary", "description", "servers", "parameters"}

// specOperations son las operaciones del contrato ("POST /v1/transactions") con el x-aluna-permission que
// declaran, vacío si no declaran ninguno. Se leen aquí por separado de NewAuthorizer y sin su lista de
// métodos: toda clave de una ruta que no está en notOperations cuenta, así que un método que el
// autorizador no conoce (head, options, trace) hace fallar la comparación.
func specOperations(t *testing.T) map[string]string {
	t.Helper()
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(contract.OpenAPI, &doc); err != nil {
		t.Fatal(err)
	}
	ops := map[string]string{}
	for path, item := range doc.Paths {
		for key, value := range item {
			if slices.Contains(notOperations, key) {
				continue
			}
			op, _ := value.(map[string]any)
			permission, _ := op["x-aluna-permission"].(string)
			ops[strings.ToUpper(key)+" "+path] = permission
		}
	}
	return ops
}

// TestEveryOperationDeclaresAPermission (FF-8): toda operación del contrato declara x-aluna-permission, y
// el autorizador tiene ese permiso para cada una.
func TestEveryOperationDeclaresAPermission(t *testing.T) {
	ops := specOperations(t)
	if len(ops) == 0 {
		t.Fatal("el contrato no tiene operaciones")
	}
	for op, permission := range ops {
		if permission == "" {
			t.Errorf("%s no declara x-aluna-permission", op)
		}
	}

	if got := testAuthorizer(t).permissions; !maps.Equal(got, ops) {
		t.Fatalf("permisos del autorizador = %v, el contrato declara %v", got, ops)
	}
}

// TestThePermissionTableMatchesTheMux: cada operación de la tabla del autorizador es el patrón con que el
// mux de Routes enruta su request; si no, el permiso no se encontraría y la ruta se negaría.
func TestThePermissionTableMatchesTheMux(t *testing.T) {
	mux := http.NewServeMux()
	Routes(API{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))(mux)

	for op := range testAuthorizer(t).permissions {
		method, path, _ := strings.Cut(op, " ")
		req := httptest.NewRequest(method, withIDs(path), nil)
		if _, pattern := mux.Handler(req); pattern != op {
			t.Errorf("%s llega al mux como %q", op, pattern)
		}
	}
}

// withIDs es path con un ID nuevo en cada parámetro ({id}, {hold_id}).
func withIDs(path string) string {
	return pathParam.ReplaceAllStringFunc(path, func(string) string { return ids.New().String() })
}

var pathParam = regexp.MustCompile(`\{[^}]+\}`)

// TestTheRolesOfThePolicy: config/roles.yaml define los seis roles de ADR-0014 y solo da permisos que el
// contrato declara: un permiso mal escrito no queda dando nada sin que nadie lo note.
func TestTheRolesOfThePolicy(t *testing.T) {
	policy, err := access.ParsePolicy(config.Roles)
	if err != nil {
		t.Fatal(err)
	}
	declared := slices.Collect(maps.Values(specOperations(t)))

	if want := []string{"auditor", "channel", "compliance", "finance", "operator", "system"}; !slices.Equal(policy.Roles(), want) {
		t.Errorf("roles = %v, se esperaba %v", policy.Roles(), want)
	}
	for _, permission := range policy.Permissions() {
		if !slices.Contains(declared, permission) {
			t.Errorf("config/roles.yaml da %s, que ninguna operación del contrato declara", permission)
		}
	}
}

// TestAnOperationWithoutPermissionIsRejected: un contrato con una operación sin permiso no se autoriza.
func TestAnOperationWithoutPermissionIsRejected(t *testing.T) {
	spec := []byte("paths:\n  /v1/x:\n    get:\n      operationId: x\n")

	if _, err := NewAuthorizer(spec, &access.Policy{}); !errors.Is(err, errNoPermission) {
		t.Fatalf("= %v, se esperaba errNoPermission", err)
	}
}
