package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func accountingStreamClient(t *testing.T, wire, body string) *Client {
	t.Helper()
	c, err := NewClient(Config{BaseURL: "https://example.invalid/v1", Wire: wire, HTTPClient: &http.Client{Transport: auditTransport{body}}, OneAttempt: true})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestChatStreamKeepsUsageBeforeAndAlongsideError(t *testing.T) {
	for _, sameChunk := range []bool{false, true} {
		body := `data: {"choices":[{"delta":{"content":"partial"}}],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30}}}` + "\n\n"
		if sameChunk {
			body = `data: {"error":{"message":"failed"},"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30}}}` + "\n\n"
		} else {
			body += `data: {"error":{"message":"failed"}}` + "\n\n"
		}
		result, err := accountingStreamClient(t, "chat", body).Stream(context.Background(), CompletionRequest{Model: "m"}, nil)
		if err == nil || result.PromptTokens != 100 || result.CompletionTok != 20 || result.CachedTokens != 30 || !result.PromptReported || !result.CompletionReported || !result.CacheReported {
			t.Fatalf("sameChunk=%t: partial usage lost: result=%+v error=%v", sameChunk, result, err)
		}
	}
}

func TestPartialUsageFramesPreservePreviouslyReportedCacheOnError(t *testing.T) {
	for _, test := range []struct{ wire, body string }{
		{"chat", `data: {"usage":{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":30}}}` + "\n\n" +
			`data: {"usage":{"completion_tokens":20},"error":{"message":"failed"}}` + "\n\n"},
		{"responses", `data: {"type":"response.in_progress","response":{"status":"in_progress","usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":30}}}}` + "\n\n" +
			`data: {"type":"response.failed","response":{"status":"failed","usage":{"output_tokens":20},"error":{"message":"failed"}}}` + "\n\n"},
	} {
		result, err := accountingStreamClient(t, test.wire, test.body).Stream(context.Background(), CompletionRequest{Model: "m"}, nil)
		if err == nil || result.PromptTokens != 100 || result.CompletionTok != 20 || result.CachedTokens != 30 || !result.CacheReported || !result.PromptReported || !result.CompletionReported {
			t.Fatalf("wire=%s lost partial usage/cache: result=%+v error=%v", test.wire, result, err)
		}
	}
}

func TestUsagePresenceDistinguishesZeroAndMissingAcrossWires(t *testing.T) {
	for _, test := range []struct {
		wire, body    string
		input, output bool
	}{
		{"chat", `data: {"usage":{"prompt_tokens":0}}` + "\n\ndata: [DONE]\n\n", true, false},
		{"chat", `data: {"usage":{"prompt_tokens":0,"completion_tokens":0}}` + "\n\ndata: [DONE]\n\n", true, true},
		{"responses", `data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":0}}}` + "\n\n", true, false},
		{"responses", `data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":0,"output_tokens":0}}}` + "\n\n", true, true},
		{"anthropic", `data: {"type":"message_start","message":{"usage":{"input_tokens":0,"output_tokens":0}}}` + "\n\ndata: {\"type\":\"message_stop\"}\n\n", true, false},
		{"anthropic", `data: {"type":"message_start","message":{"usage":{"input_tokens":0}}}` + "\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":0}}\n\ndata: {\"type\":\"message_stop\"}\n\n", true, true},
	} {
		result, err := accountingStreamClient(t, test.wire, test.body).Stream(context.Background(), CompletionRequest{Model: "m"}, nil)
		if err != nil || result.PromptReported != test.input || result.CompletionReported != test.output {
			t.Fatalf("wire=%s result=%+v error=%v", test.wire, result, err)
		}
	}
}

func TestAnthropicCancellationPreservesInputWithoutFinalOutputUsage(t *testing.T) {
	body := `data: {"type":"message_start","message":{"usage":{"input_tokens":100,"output_tokens":0}}}` + "\n\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial output"}}` + "\n\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := accountingStreamClient(t, "anthropic", body).Stream(ctx, CompletionRequest{Model: "m"}, func(string) { cancel() })
	if !errors.Is(err, context.Canceled) || result.PromptTokens != 100 || !result.PromptReported || result.CompletionReported || result.Text != "partial output" {
		t.Fatalf("partial accounting=%+v error=%v", result, err)
	}
}

func TestResponsesReplaysCompleteItemsWithoutDuplicatingTextAndTools(t *testing.T) {
	reasoning := `{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"summary"}],"encrypted_content":"opaque-state","future_field":{"n":9007199254740993}}`
	message := `{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"visible"}]}`
	call := `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"Read","arguments":"{}"}`
	body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"visible\"}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":2,\"item\":" + call + "}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":" + reasoning + "}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":" + message + "}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	c := accountingStreamClient(t, "responses", body)
	result, err := c.Stream(context.Background(), CompletionRequest{Model: "m"}, nil)
	if err != nil || result.ProviderWire != "responses" || len(result.ProviderContent) != 3 || string(result.ProviderContent[0]) != reasoning || len(result.ToolCalls) != 1 {
		t.Fatalf("items=%+v error=%v", result, err)
	}
	history := CompletionRequest{Model: "m", Messages: []ChatMessage{
		{Role: "assistant", Content: result.Text, ToolCalls: result.ToolCalls, ProviderOrigin: result.ProviderOrigin, ProviderWire: result.ProviderWire, ProviderContent: result.ProviderContent},
		{Role: "tool", ToolCallID: "call_1", Content: "contents"},
	}}
	wire, err := c.responsesBody(history)
	if err != nil {
		t.Fatal(err)
	}
	var request responsesBodyRequest
	if err := json.Unmarshal(wire, &request); err != nil {
		t.Fatal(err)
	}
	if len(request.Input) != 4 || len(request.Include) != 1 || request.Include[0] != "reasoning.encrypted_content" {
		t.Fatalf("duplicated or incomplete replay: %s", wire)
	}
	for i, want := range []string{reasoning, message, call} {
		if string(request.Input[i]) != want {
			t.Fatalf("item %d changed: %s", i, wire)
		}
	}
	for _, build := range []func(CompletionRequest) ([]byte, error){c.chatBody, c.anthropicBody} {
		other, err := build(history)
		if err != nil || strings.Contains(string(other), "opaque-state") || !strings.Contains(string(other), "visible") || !strings.Contains(string(other), "call_1") {
			t.Fatalf("wire switch replay=%s error=%v", other, err)
		}
	}
}

func TestResponsesPartialOpaqueReplayRetainsGenericOutput(t *testing.T) {
	c := accountingStreamClient(t, "responses", "")
	req := CompletionRequest{Model: "m", Messages: []ChatMessage{{Role: "assistant", Content: "partial text", ProviderOrigin: c.providerOrigin, ProviderWire: "responses",
		ProviderContent: []json.RawMessage{json.RawMessage(`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"opaque-state"}`)},
		ToolCalls:       []ToolCall{{ID: "call_1", Function: Function{Name: "Read", Arguments: "{}"}}},
	}}}
	body, err := c.responsesBody(req)
	if err != nil {
		t.Fatal(err)
	}
	var request responsesBodyRequest
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	if len(request.Input) != 3 || !strings.Contains(string(body), "partial text") || !strings.Contains(string(body), "call_1") || !strings.Contains(string(body), "opaque-state") {
		t.Fatalf("partial replay lost generic output: %s", body)
	}
}
