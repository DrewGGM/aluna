package access

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"slices"

	"go.yaml.in/yaml/v3"
)

// Scope is the scope with which a role grants a permission: over any resource of the token's legal entity,
// or only over those of its party_id (TH-3).
type Scope int

// Permission scopes; noScope is the scope of a permission not held.
const (
	noScope Scope = iota
	ScopeAll
	ScopeOwn
)

var (
	// ErrForbidden is a permission that none of the principal's roles grants.
	ErrForbidden = errors.New("permission denied")
	// ErrInvalidPolicy is a role policy that cannot be loaded.
	ErrInvalidPolicy = errors.New("invalid role policy")
)

// Policy holds the permissions of each role (ADR-0014): deny-by-default, whatever no role grants is denied.
type Policy struct {
	grants map[string]map[string]Scope // role → permission → scope
}

// ParsePolicy reads a policy: per role, the permissions with scope all and those with scope own
// (config/roles.yaml). An unknown field or a permission with both scopes makes it invalid.
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
					return nil, fmt.Errorf("%w: %s grants an empty permission", ErrInvalidPolicy, role)
				}
				if other, dup := grants[perm]; dup && other != scope {
					return nil, fmt.Errorf("%w: %s grants %s with both scopes", ErrInvalidPolicy, role, perm)
				}
				grants[perm] = scope
			}
		}
		p.grants[role] = grants
	}
	return p, nil
}

// Authorize is the scope with which principal pr holds permission: the widest among its roles. A role the
// policy does not define grants nothing.
func (p *Policy) Authorize(pr Principal, permission string) (Scope, error) {
	granted := noScope
	for _, role := range pr.Roles {
		switch p.grants[role][permission] {
		case ScopeAll:
			return ScopeAll, nil
		case ScopeOwn:
			granted = ScopeOwn
		case noScope: // the role does not grant the permission
		}
	}
	if granted == noScope {
		return noScope, fmt.Errorf("%w: %s", ErrForbidden, permission)
	}
	return granted, nil
}

// Roles are the roles the policy defines, sorted.
func (p *Policy) Roles() []string {
	return slices.Sorted(maps.Keys(p.grants))
}

// Permissions are the permissions some role grants, sorted and without duplicates.
func (p *Policy) Permissions() []string {
	all := map[string]bool{}
	for _, grants := range p.grants {
		for perm := range grants {
			all[perm] = true
		}
	}
	return slices.Sorted(maps.Keys(all))
}
