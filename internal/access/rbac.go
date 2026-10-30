package access

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"slices"

	"go.yaml.in/yaml/v3"
)

// Scope es el alcance con que un rol da un permiso: sobre cualquier recurso de la entidad legal del token,
// o solo sobre los de su party_id (TH-3).
type Scope int

// Alcances de un permiso; noScope es el de un permiso que no se tiene.
const (
	noScope Scope = iota
	ScopeAll
	ScopeOwn
)

var (
	// ErrForbidden es un permiso que ningún rol del principal da.
	ErrForbidden = errors.New("permiso denegado")
	// ErrInvalidPolicy es una política de roles que no se puede cargar.
	ErrInvalidPolicy = errors.New("política de roles inválida")
)

// Policy son los permisos de cada rol (ADR-0014): deny-by-default, lo que no da ningún rol se niega.
type Policy struct {
	grants map[string]map[string]Scope // rol → permiso → alcance
}

// ParsePolicy lee una política: por rol, los permisos con alcance all y los con alcance own
// (config/roles.yaml). Un campo desconocido o un permiso con los dos alcances la invalidan.
func ParsePolicy(doc []byte) (*Policy, error) {
	var roles map[string]struct {
		All []string `yaml:"all"`
		Own []string `yaml:"own"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(doc))
	dec.KnownFields(true)
	if err := dec.Decode(&roles); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidPolicy, err)
	}
	p := &Policy{grants: make(map[string]map[string]Scope, len(roles))}
	for role, g := range roles {
		grants := map[string]Scope{}
		for _, scoped := range []struct {
			scope Scope
			perms []string
		}{{ScopeAll, g.All}, {ScopeOwn, g.Own}} {
			scope := scoped.scope
			for _, perm := range scoped.perms {
				if perm == "" {
					return nil, fmt.Errorf("%w: %s da un permiso vacío", ErrInvalidPolicy, role)
				}
				if other, dup := grants[perm]; dup && other != scope {
					return nil, fmt.Errorf("%w: %s da %s con los dos alcances", ErrInvalidPolicy, role, perm)
				}
				grants[perm] = scope
			}
		}
		p.grants[role] = grants
	}
	return p, nil
}

// Authorize es el alcance con que el principal pr tiene permission: el más amplio entre sus roles. Un rol
// que la política no define no da nada.
func (p *Policy) Authorize(pr Principal, permission string) (Scope, error) {
	granted := noScope
	for _, role := range pr.Roles {
		switch p.grants[role][permission] {
		case ScopeAll:
			return ScopeAll, nil
		case ScopeOwn:
			granted = ScopeOwn
		case noScope: // el rol no da el permiso
		}
	}
	if granted == noScope {
		return noScope, fmt.Errorf("%w: %s", ErrForbidden, permission)
	}
	return granted, nil
}

// Roles son los roles que define la política, ordenados.
func (p *Policy) Roles() []string {
	return slices.Sorted(maps.Keys(p.grants))
}

// Permissions son los permisos que da algún rol, ordenados y sin repetir.
func (p *Policy) Permissions() []string {
	all := map[string]bool{}
	for _, grants := range p.grants {
		for perm := range grants {
			all[perm] = true
		}
	}
	return slices.Sorted(maps.Keys(all))
}
