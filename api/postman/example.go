package main

import (
	"fmt"
	"regexp"
)

const zeroUUID = "00000000-0000-0000-0000-000000000000"

// example builds the value of the property or parameter called name: the schema's example: in the
// contract if it has one, and otherwise a placeholder built from its type.
func (c *contract) example(name string, s *schema, depth int) (any, error) {
	switch {
	case depth > maxDepth:
		return nil, fmt.Errorf("%s: nested more than %d levels: a $ref cycle?", name, maxDepth)
	case s == nil:
		return nil, fmt.Errorf("%s: %w", name, errNoSchema)
	case s.Example != nil:
		return s.Example, nil
	case s.Ref != "":
		resolved, err := c.ref(s.Ref)
		if err != nil {
			return nil, err
		}
		return c.example(name, resolved, depth+1)
	}
	return c.typedExample(name, s, depth)
}

// typedExample fills the required properties of an object and minItems items of an array, and gives a
// scalar a value by enum, format or type.
func (c *contract) typedExample(name string, s *schema, depth int) (any, error) {
	switch s.Type {
	case "object":
		return c.objectExample(s, depth)
	case "array":
		return c.arrayExample(name, s, depth)
	case "string":
		return stringExample(name, s)
	case "integer":
		if s.Minimum != nil {
			return *s.Minimum, nil
		}
		return 0, nil
	case "boolean":
		return false, nil
	case "":
		if s.GoType == "json.RawMessage" { // free-form JSON
			return map[string]any{}, nil
		}
	}
	return nil, fmt.Errorf("%s: no example for type %q: add an example: to the contract", name, s.Type)
}

func (c *contract) objectExample(s *schema, depth int) (map[string]any, error) {
	out := make(map[string]any, len(s.Required))
	for _, name := range s.Required {
		prop, ok := s.Properties[name]
		if !ok {
			return nil, fmt.Errorf("required property %q is not defined", name)
		}
		v, err := c.example(name, prop, depth+1)
		if err != nil {
			return nil, err
		}
		out[name] = v
	}
	return out, nil
}

func (c *contract) arrayExample(name string, s *schema, depth int) ([]any, error) {
	item, err := c.example(name, s.Items, depth+1)
	if err != nil {
		return nil, err
	}
	out := make([]any, max(s.MinItems, 1))
	for i := range out {
		out[i] = item
	}
	return out, nil
}

// stringExample is the first value of an enum, a fixed value by format or else the name itself, which
// must match the pattern if there is one.
func stringExample(name string, s *schema) (string, error) {
	v := name
	switch {
	case len(s.Enum) > 0:
		v = s.Enum[0]
	case s.Format == "uuid":
		v = zeroUUID
	case s.Format == "date":
		v = "2026-01-02"
	case s.Format == "date-time":
		v = "2026-01-02T15:04:05Z"
	}
	if s.Pattern == "" {
		return v, nil
	}
	ok, err := regexp.MatchString(s.Pattern, v)
	if err != nil {
		return "", fmt.Errorf("%s: pattern %q: %w", name, s.Pattern, err)
	}
	if !ok {
		return "", fmt.Errorf("%s: %q does not match %q: add an example: to the contract", name, v, s.Pattern)
	}
	return v, nil
}
