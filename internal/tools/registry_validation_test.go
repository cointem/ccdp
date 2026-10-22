package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLeaseFreezesValidatorAndDefaultWithImplementation(t *testing.T) {
	registry := NewRegistry()
	makeSchema := func(value string) map[string]any {
		return map[string]any{"type": "object", "properties": map[string]any{
			"value": map[string]any{"type": "string", "const": value, "default": value},
		}}
	}
	original := makeSchema("old")
	registry.Register(&definitionTool{name: "versioned", schema: original})
	old := registry.Acquire()
	original["properties"].(map[string]any)["value"] = map[string]any{"const": "mutated"}
	registry.Register(&definitionTool{name: "versioned", schema: makeSchema("new")})
	for _, pair := range []struct {
		lease *Lease
		want  string
	}{{old, "old"}, {registry.Acquire(), "new"}} {
		args, err := pair.lease.PrepareArguments("versioned", nil)
		if err != nil || args["value"] != pair.want {
			t.Fatalf("default not frozen: %#v %v", args, err)
		}
		if _, err := pair.lease.PrepareArguments("versioned", map[string]any{"value": "mutated"}); err == nil {
			t.Fatal("caller mutation changed the compiled validator")
		}
	}
}

func TestInvalidSchemaOnlyDisablesItsTool(t *testing.T) {
	registry := NewRegistry()
	registry.Register(&definitionTool{name: "bad", schema: map[string]any{"$ref": "remote.json"}})
	registry.Register(&definitionTool{name: "good", schema: map[string]any{"type": "object"}})
	lease := registry.Acquire()
	if _, err := lease.PrepareArguments("bad", nil); err == nil || !strings.Contains(err.Error(), "bad input schema") {
		t.Fatalf("missing tool-local schema error: %v", err)
	}
	if _, err := lease.PrepareArguments("good", nil); err != nil {
		t.Fatalf("one broken tool disabled another: %v", err)
	}
	if declarations := lease.SchemasFiltered(nil); len(declarations) != 1 || declarations[0]["function"].(map[string]any)["name"] != "good" {
		t.Fatalf("broken schema leaked into provider declarations: %#v", declarations)
	}
}

func TestBuiltinDefaultsArePublishedAndPrepared(t *testing.T) {
	registry := NewRegistry()
	registry.Register(NewReadTool())
	registry.Register(NewAgentTool("SpawnAgent"))
	args, err := registry.Acquire().PrepareArguments("Read", map[string]any{"file_path": "example.go"})
	if err != nil || args["offset"] != json.Number("1") || args["limit"] != json.Number("2000") {
		t.Fatalf("Read defaults: %#v %v", args, err)
	}
	spawn, err := registry.Acquire().PrepareArguments("SpawnAgent", map[string]any{"task": "inspect"})
	if err != nil || spawn["role"] != "worker" || spawn["context"] != "fresh" {
		t.Fatalf("SpawnAgent defaults: %#v %v", spawn, err)
	}
	if _, exists := spawn["workspace"]; exists {
		t.Fatal("workspace must be selected from the role by the child runtime")
	}
}
