package mcp

import (
	"encoding/json"
	"testing"

	"ccdp/internal/tools"
)

func TestMCPValidatesOriginalSchemaRootsAndHostMetadataSeparately(t *testing.T) {
	document := map[string]any{
		"type": "object", "additionalProperties": false,
		"$defs":      map[string]any{"ID": map[string]any{"type": "integer", "minimum": json.Number("9007199254740993")}},
		"properties": map[string]any{"id": map[string]any{"$ref": "#/$defs/ID"}},
		"required":   []string{"id"},
	}
	tool := &mcpTool{client: &Client{name: "schema-test"}, def: ToolDef{Name: "echo", InputSchema: document, OutputSchema: document}}
	registry := tools.NewRegistry()
	registry.Register(tool)
	lease := registry.Acquire()
	args := map[string]any{"id": json.Number("9007199254740993"), "requested_capabilities": []any{map[string]any{"kind": "network"}}}
	if _, err := lease.PrepareArguments(tool.Name(), args); err != nil {
		t.Fatalf("host metadata broke the original input schema: %v", err)
	}
	if err := lease.ValidateOutput(tool.Name(), json.RawMessage(`{"id":9007199254740993}`)); err != nil {
		t.Fatalf("output references lost their original root: %v", err)
	}
	if err := lease.ValidateOutput(tool.Name(), json.RawMessage(`{"id":"9007199254740993"}`)); err == nil {
		t.Fatal("invalid MCP structuredContent accepted")
	}
	if _, changed := document["properties"].(map[string]any)["requested_capabilities"]; changed {
		t.Fatal("mutated the server's schema")
	}
}
