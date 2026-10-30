// Package api es el contrato de la API (ADR-0010). El servidor se genera de él y la autorización lee de él
// el permiso de cada operación.
package api

import _ "embed"

// OpenAPI es api/openapi.yaml.
//
//go:embed openapi.yaml
var OpenAPI []byte
