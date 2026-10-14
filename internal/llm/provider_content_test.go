package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProviderAnthropicPreservesThinkingAcrossTools(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		events := []string{
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"reason before calling tool"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"opaque-"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"signature-123"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"opaque-redacted-456"}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call1","name":"Read"}}`,
			`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
			`{"type":"content_block_stop","index":2}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
			`{"type":"message_stop"}`,
		}
		for _, event := range events {
			w.Write([]byte("data: " + event + "\n\n"))
		}
	}))
	defer srv.Close()
	c, err := NewClient(Config{BaseURL: srv.URL, Model: "m", Wire: "anthropic", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.StreamWithReasoning(context.Background(), CompletionRequest{Model: "m", ReasoningEffort: "high"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ProviderContent) != 3 {
		t.Fatalf("expected ordered complete content blocks: %s", res.ProviderContent)
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("expected tool call: %+v", res)
	}
	body, err := c.anthropicBody(CompletionRequest{Model: "m", ReasoningEffort: "high", Messages: []ChatMessage{
		{Role: "user", Content: "read the file"},
		{Role: "assistant", Content: res.Text, ReasoningContent: res.Reasoning, ToolCalls: res.ToolCalls, ProviderOrigin: res.ProviderOrigin, ProviderWire: res.ProviderWire, ProviderContent: res.ProviderContent},
		{Role: "tool", Content: "file contents", ToolCallID: "call1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"reason before calling tool", "opaque-signature-123", "opaque-redacted-456"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("tool continuation lost Anthropic thinking field %q: %s", want, body)
		}
	}
}

func TestProviderResponsesPreservesVerbosity(t *testing.T) {
	c, err := NewClient(Config{BaseURL: "https://api.openai.com/v1", Model: "gpt-5", Wire: "responses"})
	if err != nil {
		t.Fatal(err)
	}
	body, err := c.responsesBody(CompletionRequest{Model: "gpt-5", Verbosity: "low", Messages: []ChatMessage{{Role: "user", Content: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"verbosity":"low"`) {
		t.Errorf("explicit verbosity=low vanished from Responses request: %s", body)
	}
}

func TestProviderOpaqueBlocksDoNotCrossWire(t *testing.T) {
	c, _ := NewClient(Config{BaseURL: "https://example.com", Model: "m", Wire: "chat"})
	req := CompletionRequest{Model: "m", Messages: []ChatMessage{{Role: "assistant", Content: "visible", ProviderWire: "anthropic", ProviderContent: []json.RawMessage{json.RawMessage(`{"type":"thinking","thinking":"private","signature":"secret"}`)}}}}
	for _, build := range []func(CompletionRequest) ([]byte, error){c.chatBody, c.responsesBody} {
		body, err := build(req)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "secret") || strings.Contains(string(body), "private") {
			t.Fatalf("opaque content crossed wire: %s", body)
		}
	}
}

func TestProviderReplayPreservesLargeIntegerInput(t *testing.T) {
	c, _ := NewClient(Config{BaseURL: "https://example.com", Model: "m", Wire: "anthropic"})
	body, err := c.anthropicBody(CompletionRequest{Model: "m", Messages: []ChatMessage{{Role: "assistant", ProviderOrigin: c.providerOrigin, ProviderWire: "anthropic", ProviderContent: []json.RawMessage{json.RawMessage(`{"type":"tool_use","id":"call","name":"Read","input":{"n":9007199254740993}}`)}}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"n":9007199254740993`) {
		t.Fatalf("integer input changed: %s", body)
	}
}

func TestProviderContentCopiesAreIndependent(t *testing.T) {
	original := StreamResult{ProviderWire: "anthropic", ProviderContent: []json.RawMessage{json.RawMessage(`{"type":"thinking","signature":"signed"}`)}}
	cloned := CloneStreamResult(original)
	cloned.ProviderContent[0][0] = 'x'
	if !json.Valid(original.ProviderContent[0]) {
		t.Fatal("stream result clone aliases provider blocks")
	}
}
