// Command postman writes, from the API contract, the Postman collection of the API and its development
// environment into the directory it receives: go generate runs it on docs/postman/.
package main

//go:generate go run . ../../docs/postman

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/DrewGGM/aluna/api"
)

// File names in docs/postman/.
const (
	collectionFile  = "aluna.postman_collection.json"
	environmentFile = "aluna-dev.postman_environment.json"
)

// The Postman environment format.
type (
	environment struct {
		Name   string     `json:"name"`
		Values []envValue `json:"values"`
		Scope  string     `json:"_postman_variable_scope"`
	}
	envValue struct {
		Key     string `json:"key"`
		Value   string `json:"value"`
		Type    string `json:"type"`
		Enabled bool   `json:"enabled"`
	}
)

// devEnvironment points at the api and the Keycloak of deploy/compose.yaml, with the test user of
// deploy/keycloak/realm-aluna.json. The scripts of the collection fill the empty values.
var devEnvironment = environment{
	Name: "Aluna dev",
	Values: []envValue{
		{Key: "base_url", Value: "http://localhost:8080", Type: "default", Enabled: true},
		{Key: "oidc_issuer", Value: "http://localhost:8180/realms/aluna", Type: "default", Enabled: true},
		{Key: "client_id", Value: "aluna-dev", Type: "default", Enabled: true},
		{Key: "username", Value: "operator", Type: "default", Enabled: true},
		{Key: "password", Value: "operator", Type: "secret", Enabled: true},
		{Key: "access_token", Value: "", Type: "secret", Enabled: true},
		{Key: "access_token_expires_at", Value: "", Type: "default", Enabled: true},
		{Key: "idempotency_key", Value: "", Type: "default", Enabled: true},
	},
	Scope: "environment",
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: postman <output directory>")
	}
	out, err := files(api.OpenAPI)
	if err != nil {
		return err
	}
	for name, content := range out {
		//nolint:gosec // G306: committed documentation, readable by everyone
		if err := os.WriteFile(filepath.Join(args[0], name), content, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	return nil
}

// files returns the content of each file of docs/postman/ by name, built from the OpenAPI contract.
func files(openAPI []byte) (map[string][]byte, error) {
	c, err := parseContract(openAPI)
	if err != nil {
		return nil, err
	}
	col, err := newCollection(c)
	if err != nil {
		return nil, err
	}
	colJSON, err := marshal(col)
	if err != nil {
		return nil, err
	}
	envJSON, err := marshal(devEnvironment)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{collectionFile: colJSON, environmentFile: envJSON}, nil
}

// marshal writes v with a 2-space indent and a trailing newline, without escaping <, > and &.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("encode JSON: %w", err)
	}
	return buf.Bytes(), nil
}
