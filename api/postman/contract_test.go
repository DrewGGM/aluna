package main

import (
	"strings"
	"testing"
)

// TestFilesRejectsWhatItCannotTurnIntoARequest feeds files small contracts that each break one rule: the
// generation fails, naming why, instead of writing a request that does not work.
func TestFilesRejectsWhatItCannotTurnIntoARequest(t *testing.T) {
	tests := map[string]struct{ paths, schemas, want string }{
		"unresolved schema $ref": {
			paths: postThing(`{$ref: "#/components/schemas/Missing"}`),
			want:  `unresolved schema "#/components/schemas/Missing"`,
		},
		"unresolved parameter $ref": {
			paths: `/v1/things: {get: {parameters: [{$ref: "#/components/parameters/Missing"}]}}`,
			want:  `unresolved parameter "#/components/parameters/Missing"`,
		},
		"header other than Idempotency-Key": {
			paths: `/v1/things: {get: {parameters: [{name: X-Trace, in: header, schema: {type: string}}]}}`,
			want:  "no value for the header X-Trace",
		},
		"path outside /v1": {
			paths: `/healthz: {get: {summary: Health}}`,
			want:  "path /healthz is not under /v1",
		},
		"string pattern without an example": {
			paths: postThing(`{type: object, required: [code], properties: {code: {type: string, pattern: "^[0-9]+$"}}}`),
			want:  `code: "code" does not match "^[0-9]+$"`,
		},
		"untyped schema that is not raw JSON": {
			paths: postThing(`{type: object, required: [blob], properties: {blob: {description: anything}}}`),
			want:  `blob: no example for type ""`,
		},
		"$ref cycle": {
			paths:   postThing(`{$ref: "#/components/schemas/Loop"}`),
			schemas: `Loop: {type: object, required: [next], properties: {next: {$ref: "#/components/schemas/Loop"}}}`,
			want:    "a $ref cycle?",
		},
		"parameter without a schema": {
			paths: `/v1/things: {get: {parameters: [{name: q, in: query}]}}`,
			want:  "parameter q: no schema",
		},
		"parameter whose example is not a scalar": {
			paths: `/v1/things: {get: {parameters: [{name: q, in: query, schema: {type: object, example: {a: 1}}}]}}`,
			want:  "parameter q: the example map[a:1] is not a scalar",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			spec := "paths:\n  " + tt.paths + "\ncomponents:\n  schemas: {" + tt.schemas + "}\n"
			_, err := files([]byte(spec))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("files = %v, want an error with %q", err, tt.want)
			}
		})
	}
}

// postThing is the paths of a contract with a POST /v1/things whose body has bodySchema.
func postThing(bodySchema string) string {
	return `/v1/things: {post: {requestBody: {content: {application/json: {schema: ` + bodySchema + `}}}}}`
}

func TestRawJSONWithoutAnExampleIsAnEmptyObject(t *testing.T) {
	spec := "paths:\n  " + postThing(`{type: object, required: [doc], properties: {doc: {x-go-type: json.RawMessage}}}`)
	out, err := files([]byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	if want := `"raw": "{\n  \"doc\": {}\n}"`; !strings.Contains(string(out[collectionFile]), want) {
		t.Errorf("collection =\n%s\nwant a body %s", out[collectionFile], want)
	}
}
