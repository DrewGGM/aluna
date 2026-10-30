package access

import (
	"errors"
	"slices"
	"testing"
)

const testPolicy = `
channel:
  own: [cuentas.leer]
operator:
  all: [cuentas.leer, asientos.crear]
auditor:
  all: [cuentas.leer]
`

func parse(t *testing.T, doc string) *Policy {
	t.Helper()
	p, err := ParsePolicy([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestAuthorizeDeniesByDefault (FR-38): un permiso vale solo si algún rol del principal lo da; un rol que
// la política no conoce, o ninguno, no da nada.
func TestAuthorizeDeniesByDefault(t *testing.T) {
	p := parse(t, testPolicy)
	cases := map[string]struct {
		roles      []string
		permission string
		want       Scope
	}{
		"rol con el permiso":               {[]string{"operator"}, "asientos.crear", ScopeAll},
		"rol sin el permiso":               {[]string{"auditor"}, "asientos.crear", noScope},
		"permiso que nadie da":             {[]string{"operator"}, "cuentas.borrar", noScope},
		"rol desconocido":                  {[]string{"admin"}, "cuentas.leer", noScope},
		"sin roles":                        {nil, "cuentas.leer", noScope},
		"canal sobre lo suyo":              {[]string{"channel"}, "cuentas.leer", ScopeOwn},
		"el rol más amplio gana":           {[]string{"channel", "auditor"}, "cuentas.leer", ScopeAll},
		"el orden de los roles no importa": {[]string{"auditor", "channel"}, "cuentas.leer", ScopeAll},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := p.Authorize(Principal{Subject: "s", Roles: tc.roles}, tc.permission)

			if tc.want == noScope && !errors.Is(err, ErrForbidden) {
				t.Fatalf("= %v, %v; se esperaba ErrForbidden", got, err)
			}
			if tc.want != noScope && (err != nil || got != tc.want) {
				t.Fatalf("= %v, %v; se esperaba %v", got, err, tc.want)
			}
		})
	}
}

// TestAnInvalidPolicyIsRejected: una política con un campo desconocido, un permiso con los dos alcances o
// que no es YAML no se carga.
func TestAnInvalidPolicyIsRejected(t *testing.T) {
	for name, doc := range map[string]string{
		"campo desconocido": "operator:\n  todos: [cuentas.leer]\n",
		"dos alcances":      "channel:\n  all: [cuentas.leer]\n  own: [cuentas.leer]\n",
		"no es YAML":        "operator: [\n",
		"permiso vacío":     "operator:\n  all: ['']\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePolicy([]byte(doc)); !errors.Is(err, ErrInvalidPolicy) {
				t.Fatalf("= %v, se esperaba ErrInvalidPolicy", err)
			}
		})
	}
}

// TestPolicyListsRolesAndPermissions: la política dice qué roles define y qué permisos da, para verificarla
// contra el contrato.
func TestPolicyListsRolesAndPermissions(t *testing.T) {
	p := parse(t, testPolicy)

	roles, permissions := p.Roles(), p.Permissions()

	if want := []string{"auditor", "channel", "operator"}; !slices.Equal(roles, want) {
		t.Errorf("roles = %v, se esperaba %v", roles, want)
	}
	if want := []string{"asientos.crear", "cuentas.leer"}; !slices.Equal(permissions, want) {
		t.Errorf("permisos = %v, se esperaba %v", permissions, want)
	}
}
