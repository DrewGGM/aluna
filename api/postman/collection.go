package main

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
)

const collectionSchema = "https://schema.getpostman.com/json/collection/v2.1.0/collection.json"

// The Postman collection v2.1 format, limited to what the collection uses.
type (
	collection struct {
		Info struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Schema      string `json:"schema"`
		} `json:"info"`
		Item  []folder `json:"item"`
		Auth  auth     `json:"auth"`
		Event []event  `json:"event"`
	}
	folder struct {
		Name string `json:"name"`
		Item []item `json:"item"`
	}
	item struct {
		Name    string  `json:"name"`
		Event   []event `json:"event,omitempty"`
		Request request `json:"request"`
	}
	request struct {
		Method      string     `json:"method"`
		Header      []header   `json:"header"`
		Body        *body      `json:"body,omitempty"`
		URL         requestURL `json:"url"`
		Description string     `json:"description"`
	}
	header struct {
		Key   string `json:"key"`
		Value string `json:"value"`
		Type  string `json:"type"`
	}
	body struct {
		Mode    string      `json:"mode"`
		Raw     string      `json:"raw"`
		Options bodyOptions `json:"options"`
	}
	bodyOptions struct {
		Raw struct {
			Language string `json:"language"`
		} `json:"raw"`
	}
	requestURL struct {
		Raw      string     `json:"raw"`
		Host     []string   `json:"host"`
		Path     []string   `json:"path"`
		Query    []urlParam `json:"query,omitempty"`
		Variable []urlParam `json:"variable,omitempty"`
	}
	urlParam struct {
		Key         string `json:"key"`
		Value       string `json:"value"`
		Description string `json:"description,omitempty"`
		Disabled    bool   `json:"disabled,omitempty"`
	}
	auth struct {
		Type   string     `json:"type"`
		Bearer []urlParam `json:"bearer"`
	}
	event struct {
		Listen string `json:"listen"`
		Script script `json:"script"`
	}
	script struct {
		Type string   `json:"type"`
		Exec []string `json:"exec"`
	}
)

const idempotencyHeader = "Idempotency-Key"

// tokenScript gets an access token from the issuer with the password grant of the environment's user
// and keeps it until 30 s before it expires.
var tokenScript = []string{
	`const expiresAt = Number(pm.environment.get("access_token_expires_at") || 0);`,
	`if (!pm.environment.get("access_token") || Date.now() >= expiresAt) {`,
	`  pm.sendRequest({`,
	`    url: pm.environment.get("oidc_issuer") + "/protocol/openid-connect/token",`,
	`    method: "POST",`,
	`    header: { "Content-Type": "application/x-www-form-urlencoded" },`,
	`    body: {`,
	`      mode: "urlencoded",`,
	`      urlencoded: [`,
	`        { key: "grant_type", value: "password" },`,
	`        { key: "client_id", value: pm.environment.get("client_id") },`,
	`        { key: "username", value: pm.environment.get("username") },`,
	`        { key: "password", value: pm.environment.get("password") }`,
	`      ]`,
	`    }`,
	`  }, (err, res) => {`,
	`    if (err || res.code !== 200) {`,
	`      console.error("token request failed", err || res.text());`,
	`      return;`,
	`    }`,
	`    const token = res.json();`,
	`    pm.environment.set("access_token", token.access_token);`,
	`    pm.environment.set("access_token_expires_at", String(Date.now() + (token.expires_in - 30) * 1000));`,
	`  });`,
	`}`,
}

// idempotencyScript gives every send of the request a new Idempotency-Key.
var idempotencyScript = []string{
	`pm.environment.set("idempotency_key", pm.variables.replaceIn("{{$guid}}"));`,
}

func newCollection(c *contract) (*collection, error) {
	folders := map[string][]item{}
	for _, path := range slices.Sorted(maps.Keys(c.Paths)) {
		name, ok := strings.CutPrefix(path, "/v1/")
		if !ok {
			return nil, fmt.Errorf("path %s is not under /v1", path)
		}
		name, _, _ = strings.Cut(name, "/")
		for _, method := range slices.Sorted(maps.Keys(c.Paths[path])) {
			it, err := newItem(c, path, method, c.Paths[path][method])
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", strings.ToUpper(method), path, err)
			}
			folders[name] = append(folders[name], it)
		}
	}
	col := &collection{
		Auth:  auth{Type: "bearer", Bearer: []urlParam{{Key: "token", Value: "{{access_token}}"}}},
		Event: []event{prerequest(tokenScript)},
	}
	col.Info.Name = c.Info.Title
	col.Info.Description = c.Info.Description + "\n\nGenerated from api/openapi.yaml by go generate: do not edit by hand."
	col.Info.Schema = collectionSchema
	for _, name := range slices.Sorted(maps.Keys(folders)) {
		col.Item = append(col.Item, folder{Name: name, Item: folders[name]})
	}
	return col, nil
}

func newItem(c *contract, path, method string, op operation) (item, error) {
	params, err := c.parameters(op)
	if err != nil {
		return item{}, err
	}
	u, err := newURL(c, path, params)
	if err != nil {
		return item{}, err
	}
	it := item{
		Name:    cmp.Or(op.Summary, op.OperationID),
		Request: request{Method: strings.ToUpper(method), Header: []header{}, URL: u, Description: description(op)},
	}
	for _, p := range params {
		if p.In != "header" {
			continue
		}
		if p.Name != idempotencyHeader {
			return item{}, fmt.Errorf("no value for the header %s", p.Name)
		}
		it.Request.Header = append(it.Request.Header, header{Key: p.Name, Value: "{{idempotency_key}}", Type: "text"})
		it.Event = []event{prerequest(idempotencyScript)}
	}
	if op.RequestBody != nil {
		if it.Request.Body, err = newBody(c, op); err != nil {
			return item{}, err
		}
		it.Request.Header = append(it.Request.Header, header{Key: "Content-Type", Value: "application/json", Type: "text"})
	}
	return it, nil
}

func description(op operation) string {
	permission := "Permission: `" + op.Permission + "`"
	if op.Description == "" {
		return permission
	}
	return op.Description + "\n\n" + permission
}

// newURL turns the path's {param} into Postman :param variables and lists the query parameters, enabled
// only when required.
func newURL(c *contract, path string, params []parameter) (requestURL, error) {
	u := requestURL{Host: []string{"{{base_url}}"}}
	var enabled []string
	for _, p := range params {
		param, err := newURLParam(c, p)
		if err != nil {
			return requestURL{}, err
		}
		switch p.In {
		case "path":
			u.Variable = append(u.Variable, param)
		case "query":
			param.Disabled = !p.Required
			u.Query = append(u.Query, param)
			if p.Required {
				enabled = append(enabled, param.Key+"="+param.Value)
			}
		}
	}
	for _, segment := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if name, ok := strings.CutPrefix(segment, "{"); ok {
			segment = ":" + strings.TrimSuffix(name, "}")
		}
		u.Path = append(u.Path, segment)
	}
	u.Raw = "{{base_url}}/" + strings.Join(u.Path, "/")
	if len(enabled) > 0 {
		u.Raw += "?" + strings.Join(enabled, "&")
	}
	return u, nil
}

// newURLParam gives p its example, which must be a scalar, and a description.
func newURLParam(c *contract, p parameter) (urlParam, error) {
	s, err := c.schema(p.Schema)
	if err != nil {
		return urlParam{}, fmt.Errorf("parameter %s: %w", p.Name, err)
	}
	value, err := c.example(p.Name, p.Schema, 0)
	if err != nil {
		return urlParam{}, err
	}
	switch value.(type) {
	case map[string]any, []any:
		return urlParam{}, fmt.Errorf("parameter %s: the example %v is not a scalar", p.Name, value)
	}
	return urlParam{Key: p.Name, Value: fmt.Sprint(value), Description: paramDescription(p, s)}, nil
}

// paramDescription falls back on what the parameter's schema says of its values.
func paramDescription(p parameter, s *schema) string {
	switch {
	case p.Description != "":
		return p.Description
	case len(s.Enum) > 0:
		return "One of: " + strings.Join(s.Enum, ", ")
	case s.Format != "":
		return "Format: " + s.Format
	case s.Pattern != "":
		return "Pattern: " + s.Pattern
	}
	return ""
}

func newBody(c *contract, op operation) (*body, error) {
	media, ok := op.RequestBody.Content["application/json"]
	if !ok || media.Schema == nil {
		return nil, fmt.Errorf("request body without an application/json schema")
	}
	value, err := c.example("body", media.Schema, 0)
	if err != nil {
		return nil, err
	}
	raw, err := marshal(value)
	if err != nil {
		return nil, err
	}
	b := &body{Mode: "raw", Raw: strings.TrimSuffix(string(raw), "\n")}
	b.Options.Raw.Language = "json"
	return b, nil
}

func prerequest(exec []string) event {
	return event{Listen: "prerequest", Script: script{Type: "text/javascript", Exec: exec}}
}
