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

// testAuthorizer is the production authorization: the policy and the contract shipped in the binary.
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

// notOperations are the keys of a contract path that are not operations (OpenAPI 3.1, Path Item).
var notOperations = []string{"$ref", "summary", "description", "servers", "parameters"}

// specOperations are the contract operations ("POST /v1/transactions") with the x-aluna-permission they
// declare, empty if they declare none. They are read here separately from NewAuthorizer and without its list
// of methods: every key of a path that is not in notOperations counts, so a method the authorizer does not
// know (head, options, trace) makes the comparison fail.
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

// TestEveryOperationDeclaresAPermission (FF-8): every contract operation declares x-aluna-permission, and the
// authorizer has that permission for each one.
func TestEveryOperationDeclaresAPermission(t *testing.T) {
	ops := specOperations(t)
	if len(ops) == 0 {
		t.Fatal("the contract has no operations")
	}
	for op, permission := range ops {
		if permission == "" {
			t.Errorf("%s does not declare x-aluna-permission", op)
		}
	}

	if got := testAuthorizer(t).permissions; !maps.Equal(got, ops) {
		t.Fatalf("authorizer permissions = %v, the contract declares %v", got, ops)
	}
}

// TestThePermissionTableMatchesTheMux: every operation in the authorizer's table is the pattern with which
// the Routes mux routes its request; otherwise, the permission would not be found and the route would be
// denied.
func TestThePermissionTableMatchesTheMux(t *testing.T) {
	mux := http.NewServeMux()
	Routes(API{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))(mux)

	for op := range testAuthorizer(t).permissions {
		method, path, _ := strings.Cut(op, " ")
		req := httptest.NewRequest(method, withIDs(path), nil)
		if _, pattern := mux.Handler(req); pattern != op {
			t.Errorf("%s reaches the mux as %q", op, pattern)
		}
	}
}

// withIDs is path with a new ID in each parameter ({id}, {hold_id}).
func withIDs(path string) string {
	return pathParam.ReplaceAllStringFunc(path, func(string) string { return ids.New().String() })
}

var pathParam = regexp.MustCompile(`\{[^}]+\}`)

// TestTheRolesOfThePolicy: config/roles.yaml defines the six roles of ADR-0014 and only grants permissions
// the contract declares: a misspelled permission does not sit there granting nothing without anyone
// noticing.
func TestTheRolesOfThePolicy(t *testing.T) {
	policy, err := access.ParsePolicy(config.Roles)
	if err != nil {
		t.Fatal(err)
	}
	declared := slices.Collect(maps.Values(specOperations(t)))

	if want := []string{"auditor", "channel", "compliance", "finance", "operator", "system"}; !slices.Equal(policy.Roles(), want) {
		t.Errorf("roles = %v, want %v", policy.Roles(), want)
	}
	for _, permission := range policy.Permissions() {
		if !slices.Contains(declared, permission) {
			t.Errorf("config/roles.yaml grants %s, which no contract operation declares", permission)
		}
	}
}

// TestAnOperationWithoutPermissionIsRejected: a contract with an operation without a permission is not
// authorized.
func TestAnOperationWithoutPermissionIsRejected(t *testing.T) {
	spec := []byte("paths:\n  /v1/x:\n    get:\n      operationId: x\n")

	if _, err := NewAuthorizer(spec, &access.Policy{}); !errors.Is(err, errNoPermission) {
		t.Fatalf("= %v, want errNoPermission", err)
	}
}
