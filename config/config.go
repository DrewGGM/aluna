// Package config es la configuración que viaja dentro del binario: un cambio pasa por revisión y por una
// versión nueva, nunca por un archivo editado en el servidor.
package config

import _ "embed"

// Roles es config/roles.yaml, la política de roles y permisos (ADR-0014).
//
//go:embed roles.yaml
var Roles []byte
