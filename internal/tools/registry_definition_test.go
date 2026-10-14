package tools

import (
	"strings"
	"testing"
)

type definitionTool struct {
	name     string
	schema   map[string]any
	metadata func()
}

func (t *definitionTool) Name() string { return t.name }
func (t *definitionTool) Description() string {
	if t.metadata != nil {
		t.metadata()
	}
	return t.name
}
func (t *definitionTool) Parameters() map[string]any { return t.schema }
func (*definitionTool) Run(*Context) (string, error) { return "", nil }

func TestRegistryDefinitionBelongsToImplementationGeneration(t *testing.T) {
	r := NewRegistry()
	calls := 0
	old := &definitionTool{name: "test", schema: map[string]any{"properties": map[string]any{"value": "old"}}}
	old.metadata = func() { calls++; _ = r.Names() } // callback can read the registry
	dispose := r.Register(old)
	lease := r.Acquire()
	old.schema["properties"].(map[string]any)["value"] = "mutated"
	r.ReplaceScope("global", []Tool{&definitionTool{name: "test", schema: map[string]any{"const": "new"}}})
	dispose() // a retired disposer cannot remove the replacement
	if got, _ := lease.Get("test"); got != old {
		t.Fatal("lease changed implementation")
	}
	definition, _ := lease.Definition("test")
	if definition.Parameters["properties"].(map[string]any)["value"] != "old" {
		t.Fatal("registration retained caller schema")
	}
	definition.Parameters["properties"].(map[string]any)["value"] = "caller"
	schemas := lease.SchemasFiltered(nil)
	if schemas[0]["function"].(map[string]any)["parameters"].(map[string]any)["properties"].(map[string]any)["value"] != "old" {
		t.Fatal("definition exposed lease storage")
	}
	if got, _ := r.Acquire().Definition("test"); got.Parameters["const"] != "new" {
		t.Fatal("replacement lost")
	}
	if calls != 1 {
		t.Fatalf("extension metadata invoked %d times", calls)
	}
}

func TestCatalogSchemaKeepsRegistrationGenerationAndDoesNotAlias(t *testing.T) {
	r := NewRegistry()
	source := map[string]any{"type": "object", "title": "old"}
	r.Register(&definitionTool{name: "schema", schema: source})
	old := r.Acquire()
	source["title"] = "mutated by extension"
	r.Register(&definitionTool{name: "schema", schema: map[string]any{"type": "object", "title": "new"}})
	first, ok := old.SchemaJSON("schema")
	if !ok || !strings.Contains(string(first), `"old"`) {
		t.Fatalf("old generation lost: %s", first)
	}
	first[0] = '!'
	again, _ := old.SchemaJSON("schema")
	if again[0] != '{' {
		t.Fatal("caller mutated shared schema")
	}
	current, _ := r.Acquire().SchemaJSON("schema")
	if !strings.Contains(string(current), `"new"`) {
		t.Fatalf("new generation missing: %s", current)
	}
}
