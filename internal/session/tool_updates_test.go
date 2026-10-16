package session

import (
	"encoding/json"
	"testing"

	"ccdp/internal/messages"
)

func TestToolUpdateCodecAndClone(t *testing.T) {
	message := messages.Message{ID: "catalog", Role: messages.RoleSystem, ToolUpdate: &messages.ToolUpdate{Initial: true, Added: []messages.ToolDefinition{{Name: "test", Parameters: map[string]any{"type": "object"}}}}}
	encoded, err := EncodeMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var durable Message
	if err := json.Unmarshal(data, &durable); err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeMessage(durable)
	if err != nil {
		t.Fatal(err)
	}
	tools, known := messages.CurrentTools([]messages.Message{decoded})
	if !known || len(tools) != 1 || tools[0].Name != "test" {
		t.Fatalf("lost update: %+v", decoded)
	}
	cloned := messages.Clone([]messages.Message{decoded})
	cloned[0].ToolUpdate.Added[0].Parameters["type"] = "string"
	if decoded.ToolUpdate.Added[0].Parameters["type"] != "object" {
		t.Fatal("catalog clone aliases schema")
	}
	encoded.ToolUpdate.Added[0].Parameters["type"] = "string"
	if message.ToolUpdate.Added[0].Parameters["type"] != "object" {
		t.Fatal("durable codec aliases schema")
	}
}
