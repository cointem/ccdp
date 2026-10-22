package toolschema

import (
	"encoding/json"
	"strings"
	"testing"
)

func schemaDocument(t *testing.T, raw string) map[string]any {
	t.Helper()
	value, err := Decode([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return value.(map[string]any)
}

func TestExactNumbersDefaultsAndNull(t *testing.T) {
	schema, err := Compile(schemaDocument(t, `{
		"type":"object","additionalProperties":false,
		"properties":{
			"id":{"type":"integer","minimum":9007199254740993,"maximum":9007199254740993},
			"limit":{"type":"integer","default":25},
			"nullable":{"type":["integer","null"],"default":3}
		},"required":["id"]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	original := map[string]any{"id": json.Number("9007199254740993"), "nullable": nil}
	prepared, err := schema.Prepare(original)
	if err != nil || prepared["id"] != original["id"] || prepared["limit"] != json.Number("25") || prepared["nullable"] != nil {
		t.Fatalf("prepared=%#v error=%v", prepared, err)
	}
	if _, changed := original["limit"]; changed {
		t.Fatal("preparation changed the original call")
	}
	for _, args := range []map[string]any{
		{"id": "9007199254740993"},
		{"id": json.Number("9007199254740992")},
		{"id": json.Number("9007199254740993"), "limit": nil},
		{"id": json.Number("9007199254740993"), "unknown": true},
	} {
		if _, err := schema.Prepare(args); err == nil {
			t.Fatalf("accepted invalid arguments %#v", args)
		}
	}
}

func TestNestedDefaultsAndLocalReferences(t *testing.T) {
	schema, err := Compile(schemaDocument(t, `{
		"$defs":{"row":{"type":"object","properties":{"count":{"type":"integer","default":7}}}},
		"type":"object","properties":{"rows":{"type":"array","items":{"$ref":"#/$defs/row"}}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	value, err := schema.Prepare(map[string]any{"rows": []any{map[string]any{}}})
	if err != nil || value["rows"].([]any)[0].(map[string]any)["count"] != json.Number("7") {
		t.Fatalf("%#v %v", value, err)
	}
}

func TestEmbeddedResourceReferencesDoNotLoadExternalDocuments(t *testing.T) {
	schema, err := Compile(schemaDocument(t, `{
		"$id":"https://example.invalid/root",
		"$defs":{"ID":{"$id":"id","type":"integer","default":9}},
		"type":"object","properties":{"id":{"$ref":"id"}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	args, err := schema.Prepare(nil)
	if err != nil || args["id"] != json.Number("9") {
		t.Fatalf("embedded document/default reference failed: %#v %v", args, err)
	}
}

func TestOutputDefaultsRemainAnnotations(t *testing.T) {
	schema, err := CompileValidation(schemaDocument(t, `{"type":"integer","default":"not an integer"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(json.Number("2")); err != nil {
		t.Fatal(err)
	}
}

func TestRejectUnsupportedSchemaWithoutExternalReads(t *testing.T) {
	for _, raw := range []string{
		`{"$ref":"https://example.invalid/schema"}`,
		`{"$ref":"relative.json#/$defs/value"}`,
		`{"$ref":"file:///etc/passwd"}`,
		`{"$schema":"https://example.invalid/custom-draft","type":"object"}`,
		`{"type":"object","properties":{"limit":{"type":"integer","default":"ten"}}}`,
		`{"oneOf":[{"properties":{"limit":{"type":"integer","default":10}}},false]}`,
	} {
		if _, err := Compile(schemaDocument(t, raw)); err == nil {
			t.Fatalf("accepted unsupported schema %s", raw)
		}
	}
}

func TestRecursiveSchemaTerminatesAndDraft2019Works(t *testing.T) {
	schema, err := Compile(schemaDocument(t, `{
		"$schema":"https://json-schema.org/draft/2019-09/schema",
		"type":"object","properties":{"value":{"type":"integer"},"next":{"$ref":"#"}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := schema.Prepare(map[string]any{"value": json.Number("1"), "next": map[string]any{"value": json.Number("2")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Decode([]byte(`{} {}`)); err == nil || !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("trailing input accepted: %v", err)
	}
}
