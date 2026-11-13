package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/DrewGGM/aluna/api"
)

const docsDir = "../../docs/postman"

// committedCollection is the part of the committed collection the tests read.
type committedCollection struct {
	Info struct {
		Schema string `json:"schema"`
	} `json:"info"`
	Auth struct {
		Type string `json:"type"`
	} `json:"auth"`
	Event []committedEvent `json:"event"`
	Item  []struct {
		Name string          `json:"name"`
		Item []committedItem `json:"item"`
	} `json:"item"`
}

type committedItem struct {
	Name    string           `json:"name"`
	Event   []committedEvent `json:"event"`
	Request struct {
		Method string            `json:"method"`
		Header []committedHeader `json:"header"`
		Body   *struct {
			Mode string `json:"mode"`
			Raw  string `json:"raw"`
		} `json:"body"`
		URL struct {
			Raw      string   `json:"raw"`
			Host     []string `json:"host"`
			Path     []string `json:"path"`
			Variable []struct {
				Key string `json:"key"`
			} `json:"variable"`
		} `json:"url"`
		Description string `json:"description"`
	} `json:"request"`
}

type committedHeader struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type committedEvent struct {
	Listen string `json:"listen"`
	Script struct {
		Exec []string `json:"exec"`
	} `json:"script"`
}

func readCollection(t *testing.T) committedCollection {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(docsDir, collectionFile))
	if err != nil {
		t.Fatal(err)
	}
	var col committedCollection
	if err := json.Unmarshal(raw, &col); err != nil {
		t.Fatalf("the collection is not JSON: %v", err)
	}
	return col
}

func TestCommittedFilesMatchTheContract(t *testing.T) {
	out, err := files(api.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range out {
		got, err := fs.ReadFile(os.DirFS(docsDir), name)
		if err != nil {
			t.Fatal(err)
		}
		// a checkout with CRLF line endings is still up to date
		if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), want) {
			t.Errorf("docs/postman/%s is out of date with api/openapi.yaml: run go generate ./api/postman", name)
		}
	}
}

func TestCollectionHasEveryOperation(t *testing.T) {
	var spec struct {
		Paths map[string]map[string]struct {
			Permission string `yaml:"x-aluna-permission"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(api.OpenAPI, &spec); err != nil {
		t.Fatal(err)
	}
	inCollection := map[string]string{}
	for _, f := range readCollection(t).Item {
		for _, it := range f.Item {
			path := strings.Join(it.Request.URL.Path, "/")
			for _, v := range it.Request.URL.Variable {
				path = strings.Replace(path, ":"+v.Key, "{"+v.Key+"}", 1)
			}
			inCollection[it.Request.Method+" /"+path] = it.Request.Description
		}
	}
	for path, ops := range spec.Paths {
		for method, op := range ops {
			key := strings.ToUpper(method) + " " + path
			desc, ok := inCollection[key]
			if !ok {
				t.Errorf("%s is missing from the collection", key)
				continue
			}
			if !strings.Contains(desc, "Permission: `"+op.Permission+"`") {
				t.Errorf("%s: the description does not name its permission %s", key, op.Permission)
			}
		}
	}
}

func TestCollectionIsPostmanV21WithABearerToken(t *testing.T) {
	col := readCollection(t)
	if col.Info.Schema != "https://schema.getpostman.com/json/collection/v2.1.0/collection.json" {
		t.Errorf("info.schema = %q, want the v2.1.0 schema", col.Info.Schema)
	}
	fetchesToken := func(line string) bool { return strings.Contains(line, "openid-connect/token") }
	if col.Auth.Type != "bearer" || len(col.Event) != 1 || col.Event[0].Listen != "prerequest" ||
		!slices.ContainsFunc(col.Event[0].Script.Exec, fetchesToken) {
		t.Errorf("the collection must send a bearer token that its pre-request script fetches: auth %q, events %+v", col.Auth.Type, col.Event)
	}
}

func TestWithdrawalRequest(t *testing.T) {
	withdrawal := findItem(t, readCollection(t), "deposit-accounts", "Withdraw cash from an account")
	req := withdrawal.Request
	if req.Method != "POST" || req.URL.Raw != "{{base_url}}/v1/deposit-accounts/:id/withdrawals" ||
		!slices.Equal(req.URL.Host, []string{"{{base_url}}"}) || len(req.URL.Variable) != 1 || req.URL.Variable[0].Key != "id" {
		t.Errorf("withdrawal request = %+v", req)
	}
	want := `{
  "amount": {
    "amount": 10000000,
    "currency": "COP"
  }
}`
	if req.Body == nil || req.Body.Mode != "raw" || req.Body.Raw != want {
		t.Errorf("withdrawal body = %+v, want raw %s", req.Body, want)
	}
}

func TestOnlyIdempotentOperationsGetAFreshIdempotencyKey(t *testing.T) {
	col := readCollection(t)
	withdrawal := findItem(t, col, "deposit-accounts", "Withdraw cash from an account")
	sendsKey := slices.ContainsFunc(withdrawal.Request.Header, func(h committedHeader) bool {
		return h.Key == "Idempotency-Key" && h.Value == "{{idempotency_key}}"
	})
	if !sendsKey || len(withdrawal.Event) != 1 || !strings.Contains(strings.Join(withdrawal.Event[0].Script.Exec, "\n"), "idempotency_key") {
		t.Errorf("the withdrawal must send a fresh Idempotency-Key: headers %+v, events %+v", withdrawal.Request.Header, withdrawal.Event)
	}
	if read := findItem(t, col, "deposit-accounts", "Read a deposit account"); len(read.Event) != 0 || len(read.Request.Header) != 0 {
		t.Errorf("a read takes no Idempotency-Key: %+v", read)
	}
}

func findItem(t *testing.T, col committedCollection, folder, name string) committedItem {
	t.Helper()
	for _, f := range col.Item {
		for _, it := range f.Item {
			if f.Name == folder && it.Name == name {
				return it
			}
		}
	}
	t.Fatalf("no request %q in folder %q", name, folder)
	return committedItem{}
}
