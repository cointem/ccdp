package tools

import (
	"testing"
)

func TestBuiltinSearchExposesOnlyRg(t *testing.T) {
	registry := NewRegistry()
	RegisterBuiltins(registry, false)
	if tool, ok := registry.Get("Rg"); !ok || tool.Name() != "Rg" {
		t.Fatal("Rg was not registered")
	}
	if _, ok := registry.Get("Grep"); ok {
		t.Fatal("obsolete Grep compatibility alias remains")
	}
	for _, schema := range registry.Acquire().SchemasFiltered(nil) {
		fn, ok := schema["function"].(map[string]any)
		if ok && fn["name"] == "Grep" {
			t.Fatal("model still sees Grep schema")
		}
	}
}
