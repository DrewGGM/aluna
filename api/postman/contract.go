package main

import (
	"errors"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// contract is the part of the OpenAPI document the collection is built from.
type contract struct {
	Info struct {
		Title       string `yaml:"title"`
		Description string `yaml:"description"`
	} `yaml:"info"`
	Paths      map[string]map[string]operation `yaml:"paths"`
	Components struct {
		Parameters map[string]parameter `yaml:"parameters"`
		Schemas    map[string]*schema   `yaml:"schemas"`
	} `yaml:"components"`
}

type operation struct {
	OperationID string      `yaml:"operationId"`
	Summary     string      `yaml:"summary"`
	Description string      `yaml:"description"`
	Permission  string      `yaml:"x-aluna-permission"`
	Parameters  []parameter `yaml:"parameters"`
	RequestBody *struct {
		Content map[string]struct {
			Schema *schema `yaml:"schema"`
		} `yaml:"content"`
	} `yaml:"requestBody"`
}

type parameter struct {
	Ref         string  `yaml:"$ref"`
	Name        string  `yaml:"name"`
	In          string  `yaml:"in"`
	Required    bool    `yaml:"required"`
	Description string  `yaml:"description"`
	Schema      *schema `yaml:"schema"`
}

type schema struct {
	Ref        string             `yaml:"$ref"`
	Type       string             `yaml:"type"`
	GoType     string             `yaml:"x-go-type"`
	Format     string             `yaml:"format"`
	Pattern    string             `yaml:"pattern"`
	Enum       []string           `yaml:"enum"`
	Minimum    *int64             `yaml:"minimum"`
	MinItems   int                `yaml:"minItems"`
	Required   []string           `yaml:"required"`
	Properties map[string]*schema `yaml:"properties"`
	Items      *schema            `yaml:"items"`
	Example    any                `yaml:"example"`
}

const (
	schemaRefPrefix    = "#/components/schemas/"
	parameterRefPrefix = "#/components/parameters/"
	// maxDepth bounds how far a schema is followed, so a $ref cycle is an error and not a hang.
	maxDepth = 32
)

var errNoSchema = errors.New("no schema")

func parseContract(openAPI []byte) (*contract, error) {
	var c contract
	if err := yaml.Unmarshal(openAPI, &c); err != nil {
		return nil, fmt.Errorf("parse the contract: %w", err)
	}
	return &c, nil
}

// ref returns the schema a $ref names.
func (c *contract) ref(ref string) (*schema, error) {
	s, ok := c.Components.Schemas[strings.TrimPrefix(ref, schemaRefPrefix)]
	if !ok || !strings.HasPrefix(ref, schemaRefPrefix) {
		return nil, fmt.Errorf("unresolved schema %q", ref)
	}
	return s, nil
}

// schema follows s's $refs to the schema that defines it.
func (c *contract) schema(s *schema) (*schema, error) {
	for range maxDepth {
		if s == nil {
			return nil, errNoSchema
		}
		if s.Ref == "" {
			return s, nil
		}
		var err error
		if s, err = c.ref(s.Ref); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("more than %d $refs in a row: a cycle?", maxDepth)
}

// parameters returns op's parameters with their $refs resolved.
func (c *contract) parameters(op operation) ([]parameter, error) {
	params := make([]parameter, 0, len(op.Parameters))
	for _, p := range op.Parameters {
		if p.Ref != "" {
			resolved, ok := c.Components.Parameters[strings.TrimPrefix(p.Ref, parameterRefPrefix)]
			if !ok || !strings.HasPrefix(p.Ref, parameterRefPrefix) {
				return nil, fmt.Errorf("unresolved parameter %q", p.Ref)
			}
			p = resolved
		}
		if p.Schema == nil {
			return nil, fmt.Errorf("parameter %s: %w", p.Name, errNoSchema)
		}
		params = append(params, p)
	}
	return params, nil
}
