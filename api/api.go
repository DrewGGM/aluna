// Package api is the API contract (ADR-0010). The server is generated from it and authorization reads from it
// the permission of each operation.
package api

import _ "embed"

// OpenAPI is api/openapi.yaml.
//
//go:embed openapi.yaml
var OpenAPI []byte
