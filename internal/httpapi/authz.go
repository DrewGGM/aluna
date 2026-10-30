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

var errNoPermission = errors.New("operación del contrato sin x-aluna-permission")

var (
	forbidden = Problem{Status: http.StatusForbidden, Code: "forbidden", Title: "Permiso insuficiente"}
	// httpMethods son los métodos de una operación del contrato (OpenAPI 3.1); el resto de las claves de una
	// ruta (parameters, summary) no son operaciones.
	httpMethods = []string{
		http.MethodGet, http.MethodPut, http.MethodPost, http.MethodDelete,
		http.MethodOptions, http.MethodHead, http.MethodPatch, http.MethodTrace,
	}
)

// Authorizer niega por defecto (ADR-0014, FR-38): una ruta del contrato exige el permiso que declara en
// x-aluna-permission y el principal lo debe tener por alguno de sus roles.
type Authorizer struct {
	policy      *access.Policy
	permissions map[string]string // patrón del mux ("GET /v1/transactions/{id}") → permiso
}

// NewAuthorizer autoriza las operaciones de spec, el contrato (api/openapi.yaml), con policy. Una operación
// sin permiso es un error: el contrato no se sirve a medias.
func NewAuthorizer(spec []byte, policy *access.Policy) (*Authorizer, error) {
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(spec, &doc); err != nil {
		return nil, fmt.Errorf("contrato de la API: %w", err)
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
				return nil, fmt.Errorf("contrato de la API, %s %s: %w", method, path, err)
			}
			if op.Permission == "" {
				return nil, fmt.Errorf("%w: %s %s", errNoPermission, method, path)
			}
			permissions[method+" "+path] = op.Permission
		}
	}
	return &Authorizer{policy: policy, permissions: permissions}, nil
}

// withAuthorization deja pasar un request con ruta solo si su principal tiene el permiso de la operación;
// si no, 403 problem+json y un log de la denegación. Los probes, los requests sin ruta (los responde el
// mux) y los que el mux redirige a su ruta limpia pasan: el request limpio se autoriza y se audita. Con
// alcance own, la operación solo puede responder recursos de la party del token, y ninguna sabe todavía de
// quién es el suyo: 404, como un recurso que no existe (TH-3, AB-4), sin correr el handler y con el mismo
// log de la denegación.
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
				logDenial(log, r, p, pattern, permission, "alcance own: ningún recurso tiene dueño todavía")
				WriteProblem(w, r, notFound)
			default:
				next.ServeHTTP(w, r)
			}
		})
	}
}

// redirectsToCleanPath dice si el mux responde a r solo con una redirección a su ruta limpia: la ruta
// escapada no es canónica (cleanPath de net/http) y el método no es CONNECT. Se mira la ruta escapada, como
// el mux: /v1/holds/a%2F..%2Fb/capture es canónica y llega al handler.
func redirectsToCleanPath(r *http.Request) bool {
	escaped := r.URL.EscapedPath()
	clean := path.Clean("/" + escaped)
	if strings.HasSuffix(escaped, "/") && clean != "/" {
		clean += "/"
	}
	return r.Method != http.MethodConnect && clean != escaped
}

// logDenial deja en log la denegación de pattern a p: ruta, permiso, razón, party, roles y request_id, para
// alertar sobre quien prueba recursos ajenos. El sub no va: según el IAM puede ser un correo; el
// request_id lleva al registro de auditoría, que sí tiene el actor.
func logDenial(log *slog.Logger, r *http.Request, p access.Principal, pattern, permission, reason string) {
	var party string
	if p.PartyID != nil {
		party = p.PartyID.String()
	}
	log.LogAttrs(r.Context(), slog.LevelWarn, "permiso denegado",
		slog.String("route", pattern),
		slog.String("permission", permission),
		slog.String("reason", reason),
		slog.String("party_id", party),
		slog.String("roles", strings.Join(p.Roles, ",")),
		slog.String("request_id", requestIDFrom(r.Context())))
}

// authorize es el permiso que exige pattern y el alcance con que p lo tiene, o la razón por la que se
// niega: sin autorizador, una ruta fuera del contrato o ningún rol que dé el permiso.
func (a *Authorizer) authorize(p access.Principal, pattern string) (permission string, scope access.Scope, denied string) {
	if a == nil {
		return "", 0, "sin autorizador"
	}
	permission, ok := a.permissions[pattern]
	if !ok {
		return "", 0, "ruta fuera del contrato"
	}
	scope, err := a.policy.Authorize(p, permission)
	if err != nil {
		return permission, 0, "ningún rol da el permiso"
	}
	return permission, scope, ""
}
