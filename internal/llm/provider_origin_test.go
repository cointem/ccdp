package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestOpaqueReplayRequiresSameEndpointAndCredentials(t *testing.T) {
	for _, wire := range []string{"responses", "anthropic"} {
		t.Run(wire, func(t *testing.T) {
			frames := []string{
				`{"type":"response.output_text.delta","delta":"visible"}`,
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"opaque-state"}}`,
				`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"Read","arguments":"{}"}}`,
				`{"type":"response.completed","response":{"status":"completed"}}`,
			}
			if wire == "anthropic" {
				frames = []string{
					`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"summary","signature":"opaque-state"}}`,
					`{"type":"content_block_stop","index":0}`,
					`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":"visible"}}`,
					`{"type":"content_block_stop","index":1}`,
					`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call_1","name":"Read","input":{}}}`,
					`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
					`{"type":"content_block_stop","index":2}`,
					`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
					`{"type":"message_stop"}`,
				}
			}
			var stream strings.Builder
			for _, frame := range frames {
				stream.WriteString("data: " + frame + "\n\n")
			}
			sourceConfig := Config{BaseURL: "https://source.invalid/v1", APIKey: "account-secret", Wire: wire, HTTPClient: &http.Client{Transport: auditTransport{stream.String()}}, OneAttempt: true}
			source, err := NewClient(sourceConfig)
			if err != nil {
				t.Fatal(err)
			}
			result, err := source.Stream(context.Background(), CompletionRequest{Model: "m"}, nil)
			if err != nil || result.ProviderOrigin == "" || strings.Contains(result.ProviderOrigin, sourceConfig.APIKey) {
				t.Fatalf("invalid origin=%q error=%v", result.ProviderOrigin, err)
			}
			request := CompletionRequest{Model: "m", Messages: []ChatMessage{
				{Role: "assistant", Content: result.Text, ToolCalls: result.ToolCalls, ProviderWire: result.ProviderWire, ProviderOrigin: result.ProviderOrigin, ProviderContent: result.ProviderContent},
				{Role: "tool", ToolCallID: "call_1", Content: "contents"},
			}}
			// Persist the provider-neutral history and reconstruct it with a new
			// client, as a resumed session does across process lifetimes.
			saved, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			var restored CompletionRequest
			if err := json.Unmarshal(saved, &restored); err != nil {
				t.Fatal(err)
			}
			for _, target := range []struct {
				name, endpoint, key string
				replay              bool
			}{
				{"same-origin", sourceConfig.BaseURL, sourceConfig.APIKey, true},
				{"different-endpoint", "https://other.invalid/v1", sourceConfig.APIKey, false},
				{"different-account", sourceConfig.BaseURL, "other-account-secret", false},
				{"missing-origin", sourceConfig.BaseURL, sourceConfig.APIKey, false},
			} {
				t.Run(target.name, func(t *testing.T) {
					client, err := NewClient(Config{BaseURL: target.endpoint, APIKey: target.key, Wire: wire})
					if err != nil {
						t.Fatal(err)
					}
					copy := restored
					copy.Messages = append([]ChatMessage(nil), restored.Messages...)
					if target.name == "missing-origin" {
						copy.Messages[0].ProviderOrigin = ""
					}
					build := client.responsesBody
					if wire == "anthropic" {
						build = client.anthropicBody
					}
					body, err := build(copy)
					if err != nil {
						t.Fatal(err)
					}
					if strings.Contains(string(body), "opaque-state") != target.replay || !strings.Contains(string(body), "visible") || !strings.Contains(string(body), "call_1") || !strings.Contains(string(body), "contents") {
						t.Fatalf("origin replay=%t portable history missing: %s", target.replay, body)
					}
					if strings.Contains(string(body), "provider_origin") || strings.Contains(string(body), sourceConfig.APIKey) {
						t.Fatalf("internal origin/credential leaked to API body: %s", body)
					}
				})
			}
		})
	}
}
