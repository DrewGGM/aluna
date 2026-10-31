// Package config is the configuration that ships inside the binary: a change goes through review and a new
// release, never through a file edited on the server.
package config

import _ "embed"

// Roles is config/roles.yaml, the role and permission policy (ADR-0014).
//
//go:embed roles.yaml
var Roles []byte
