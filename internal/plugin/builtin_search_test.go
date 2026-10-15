package plugin

import (
	"ccdp/internal/tools"
	"testing"
)

func TestBuiltinSearchExposesOnlyRg(t *testing.T) {
	registry := tools.NewRegistry()
	if err := NewToolsPlugin(false).Init(&Context{Tools: registry}); err != nil {
		t.Fatal(err)
	}
	if tool, ok := registry.Get("Rg"); !ok || tool.Name() != "Rg" {
		t.Fatal("Rg was not registered")
	}
	if _, ok := registry.Get("Grep"); ok {
		t.Fatal("obsolete Grep compatibility alias remains")
	}
	for _, schema := range registry.Schemas() {
		fn, ok := schema["function"].(map[string]any)
		if ok && fn["name"] == "Grep" {
			t.Fatal("model still sees Grep schema")
		}
	}
}
