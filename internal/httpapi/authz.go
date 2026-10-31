package httpapi

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/DrewGGM/aluna/internal/access"
)

var errNoPermission = errors.New("contract operation without x-aluna-permission")

var (
	forbidden = Problem{Status: http.StatusForbidden, Code: "forbidden", Title: "Insufficient permission"}
	// httpMethods are the methods of a contract operation (OpenAPI 3.1); the other keys of a path
	// (parameters, summary) are not operations.
	httpMethods = []string{
		http.MethodGet, http.MethodPut, http.MethodPost, http.MethodDelete,
		http.MethodOptions, http.MethodHead, http.MethodPatch, http.MethodTrace,
	}
)

// Authorizer denies by default (ADR-0014, FR-38): a contract route requires the permission it declares in
// x-aluna-permission and the principal must hold it through one of its roles.
type Authorizer struct {
	policy      *access.Policy
	permissions map[string]string // mux pattern ("GET /v1/transactions/{id}") → permission
}

// NewAuthorizer authorizes the operations of spec, the contract (api/openapi.yaml), with policy. An operation
// without a permission is an error: the contract is not served halfway.
func NewAuthorizer(spec []byte, policy *access.Policy) (*Authorizer, error) {
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(spec, &doc); err != nil {
		return nil, fmt.Errorf("api contract: %w", err)
	}
	permissions := map[string]string{}
	for path, item := range doc.Paths {
		for _, method := range httpMethods {
			node, ok := item[strings.ToLower(method)]
			if !ok {
				continue
			}
			var op struct {
				Permission string `yaml:"x-aluna-permission"`
			}
			if err := node.Decode(&op); err != nil {
				return nil, fmt.Errorf("api contract, %s %s: %w", method, path, err)
			}
			if op.Permission == "" {
				return nil, fmt.Errorf("%w: %s %s", errNoPermission, method, path)
			}
			permissions[method+" "+path] = op.Permission
		}
	}
	return &Authorizer{policy: policy, permissions: permissions}, nil
}

// withAuthorization lets a routed request through only if its principal has the operation's permission;
// otherwise, 403 problem+json and a denial log. Probes, unrouted requests (the mux answers them) and those the
// mux redirects to their clean path pass: the clean request is authorized and audited. With scope own, the
// operation may only answer resources of the token's party, and none of them knows yet who owns its
// resource: 404, like a resource that does not exist (TH-3, AB-4), without running the handler and with the
// same denial log.
func withAuthorization(a *Authorizer, routes *http.ServeMux, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, pattern := routes.Handler(r)
			if pattern == "" || r.URL.Path == pathHealthz || r.URL.Path == pathReadyz || redirectsToCleanPath(r) {
				next.ServeHTTP(w, r)
				return
			}
			p, _ := access.PrincipalFrom(r.Context())
			permission, scope, denied := a.authorize(p, pattern)
			switch {
			case denied != "":
				logDenial(log, r, p, pattern, permission, denied)
				WriteProblem(w, r, forbidden)
			case scope == access.ScopeOwn:
				logDenial(log, r, p, pattern, permission, "scope own: no resource has an owner yet")
				WriteProblem(w, r, notFound)
			default:
				next.ServeHTTP(w, r)
			}
		})
	}
}

// redirectsToCleanPath reports whether the mux answers r only with a redirect to its clean path: the escaped
// path is not canonical (net/http's cleanPath) and the method is not CONNECT. It looks at the escaped path,
// like the mux: /v1/holds/a%2F..%2Fb/capture is canonical and reaches the handler.
func redirectsToCleanPath(r *http.Request) bool {
	escaped := r.URL.EscapedPath()
	clean := path.Clean("/" + escaped)
	if strings.HasSuffix(escaped, "/") && clean != "/" {
		clean += "/"
	}
	return r.Method != http.MethodConnect && clean != escaped
}

// logDenial logs the denial of pattern to p: route, permission, reason, party, roles and request_id, to alert
// on whoever probes other parties' resources. The sub is left out: depending on the IAM it can be an email;
// the request_id leads to the audit record, which does have the actor.
func logDenial(log *slog.Logger, r *http.Request, p access.Principal, pattern, permission, reason string) {
	var party string
	if p.PartyID != nil {
		party = p.PartyID.String()
	}
	log.LogAttrs(r.Context(), slog.LevelWarn, "permission denied",
		slog.String("route", pattern),
		slog.String("permission", permission),
		slog.String("reason", reason),
		slog.String("party_id", party),
		slog.String("roles", strings.Join(p.Roles, ",")),
		slog.String("request_id", requestIDFrom(r.Context())))
}

// authorize returns the permission that pattern requires and the scope with which p holds it, or the reason
// for the denial: no authorizer, a route outside the contract or no role that grants the permission.
func (a *Authorizer) authorize(p access.Principal, pattern string) (permission string, scope access.Scope, denied string) {
	if a == nil {
		return "", 0, "no authorizer"
	}
	permission, ok := a.permissions[pattern]
	if !ok {
		return "", 0, "route outside the contract"
	}
	scope, err := a.policy.Authorize(p, permission)
	if err != nil {
		return permission, 0, "no role grants the permission"
	}
	return permission, scope, ""
}
