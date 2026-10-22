package toolschema

import (
	"encoding/json"
	"testing"
)

func TestDynamicPropertyDefaults(t *testing.T) {
	for _, raw := range []string{
		`{"type":"object","additionalProperties":{"type":"object","properties":{"n":{"type":"integer","default":7}},"required":["n"]}}`,
		`{"type":"object","patternProperties":{"^x":{"type":"object","properties":{"n":{"type":"integer","default":7}},"required":["n"]}}}`,
	} {
		s, err := Compile(schemaDocument(t, raw))
		if err != nil {
			t.Fatal(err)
		}
		args := map[string]any{"x": map[string]any{}}
		got, err := s.Prepare(args)
		if err != nil || got["x"].(map[string]any)["n"] != json.Number("7") {
			t.Fatalf("defaults missing: %#v %v", got, err)
		}
		if len(args["x"].(map[string]any)) != 0 {
			t.Fatal("mutated original arguments")
		}
	}
}

func TestDynamicPropertiesCheckDefaultsAtRegistration(t *testing.T) {
	for _, raw := range []string{
		`{"additionalProperties":{"properties":{"n":{"type":"integer","default":"bad"}}}}`,
		`{"patternProperties":{"^x":{"properties":{"n":{"type":"integer","default":"bad"}}}}}`,
		`{"additionalProperties":{"anyOf":[{"properties":{"n":{"default":7}}}]}}`,
	} {
		if _, err := Compile(schemaDocument(t, raw)); err == nil {
			t.Fatalf("accepted unsupported default: %s", raw)
		}
	}
}
