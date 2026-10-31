package access

import (
	"errors"
	"slices"
	"testing"
)

const testPolicy = `
channel:
  own: [accounts.read]
operator:
  all: [accounts.read, entries.create]
auditor:
  all: [accounts.read]
`

func parse(t *testing.T, doc string) *Policy {
	t.Helper()
	p, err := ParsePolicy([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestAuthorizeDeniesByDefault (FR-38): a permission holds only if some role of the principal grants it; a
// role the policy does not know, or none at all, grants nothing.
func TestAuthorizeDeniesByDefault(t *testing.T) {
	p := parse(t, testPolicy)
	cases := map[string]struct {
		roles      []string
		permission string
		want       Scope
	}{
		"role with the permission":      {[]string{"operator"}, "entries.create", ScopeAll},
		"role without the permission":   {[]string{"auditor"}, "entries.create", noScope},
		"permission nobody grants":      {[]string{"operator"}, "accounts.delete", noScope},
		"unknown role":                  {[]string{"admin"}, "accounts.read", noScope},
		"no roles":                      {nil, "accounts.read", noScope},
		"channel over its own":          {[]string{"channel"}, "accounts.read", ScopeOwn},
		"the widest role wins":          {[]string{"channel", "auditor"}, "accounts.read", ScopeAll},
		"the order of roles is ignored": {[]string{"auditor", "channel"}, "accounts.read", ScopeAll},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := p.Authorize(Principal{Subject: "s", Roles: tc.roles}, tc.permission)

			if tc.want == noScope && !errors.Is(err, ErrForbidden) {
				t.Fatalf("= %v, %v; want ErrForbidden", got, err)
			}
			if tc.want != noScope && (err != nil || got != tc.want) {
				t.Fatalf("= %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

// TestAnInvalidPolicyIsRejected: a policy with an unknown field, a permission with both scopes or that is
// not YAML is not loaded.
func TestAnInvalidPolicyIsRejected(t *testing.T) {
	for name, doc := range map[string]string{
		"unknown field":    "operator:\n  every: [accounts.read]\n",
		"both scopes":      "channel:\n  all: [accounts.read]\n  own: [accounts.read]\n",
		"not YAML":         "operator: [\n",
		"empty permission": "operator:\n  all: ['']\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePolicy([]byte(doc)); !errors.Is(err, ErrInvalidPolicy) {
				t.Fatalf("= %v, want ErrInvalidPolicy", err)
			}
		})
	}
}

// TestPolicyListsRolesAndPermissions: the policy says which roles it defines and which permissions it
// grants, to check it against the contract.
func TestPolicyListsRolesAndPermissions(t *testing.T) {
	p := parse(t, testPolicy)

	roles, permissions := p.Roles(), p.Permissions()

	if want := []string{"auditor", "channel", "operator"}; !slices.Equal(roles, want) {
		t.Errorf("roles = %v, want %v", roles, want)
	}
	if want := []string{"accounts.read", "entries.create"}; !slices.Equal(permissions, want) {
		t.Errorf("permissions = %v, want %v", permissions, want)
	}
}
