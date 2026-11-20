package llm

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccdp/internal/messages"
)

const inlineToolErrorBody = `{"type":"error","error":{"type":"invalid_request_error","message":"invalid params, messages.43.content.0: unsupported content type 'tool_addition' (2013)"}}`

func TestAnthropicMessagesInlineToolFallback(t *testing.T) {
	for _, oneAttempt := range []bool{false, true} {
		name := "client retry"
		if oneAttempt {
			name = "runtime retry"
		}
		t.Run(name, func(t *testing.T) {
			var attempts atomic.Int32
			bodies := make(chan []byte, 8)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "test-key" {
					t.Errorf("unexpected Messages route or authentication: %s %s", r.Method, r.URL.Path)
				}
				n := attempts.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read request: %v", err)
				}
				bodies <- body
				betas := r.Header.Get("anthropic-beta")
				wantInline := n == 1 || n == 4
				if strings.Contains(betas, anthropicInlineToolsBeta) != wantInline || !strings.Contains(betas, anthropicEffortBeta) {
					t.Errorf("attempt %d beta headers %q", n, betas)
				}
				if n == 1 {
					w.WriteHeader(http.StatusBadRequest)
					io.WriteString(w, inlineToolErrorBody)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n"+
					"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n"+
					"data: {\"type\":\"message_stop\"}\n\n")
			}))
			defer server.Close()
			client, err := NewClient(Config{Wire: "anthropic", BaseURL: server.URL + "/v1", APIKey: "test-key", OneAttempt: oneAttempt, RetryDelay: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			late := catalogTool("late", "new")
			req := catalogRequest([]ToolDef{late}, &messages.ToolUpdate{Added: toolDefinitions([]ToolDef{late}), Removed: []string{"base"}})
			req.ReasoningEffort = "high"
			req.SystemBlocks = []SystemBlock{{Text: "base instructions", Cacheable: true}}
			req.Messages = append(req.Messages,
				ChatMessage{Role: "assistant", ToolCalls: []ToolCall{{ID: "call", Function: Function{Name: "late", Arguments: "{}"}}}},
				ChatMessage{Role: "tool", ToolCallID: "call", Content: "result"},
				ChatMessage{Role: "user", Content: "follow up"})
			// The ordinary request has the exact same conversation, including the
			// tool_use/tool_result pair, but no catalog history.
			ordinary := req
			ordinary.Messages = append([]ChatMessage{req.Messages[0], req.Messages[2]}, req.Messages[4:]...)
			wantPlain, err := client.marshalRequestBody(ordinary)
			if err != nil {
				t.Fatal(err)
			}
			call, err := NewPreparedCall(client, req.Model, client.Endpoint(), req)
			if err != nil {
				t.Fatal(err)
			}
			defer call.Close()
			var deltas strings.Builder
			onDelta := func(s string) { deltas.WriteString(s) }
			if oneAttempt {
				_, err := call.Stream(context.Background(), onDelta)
				if _, retry := RetryAfter(err); !retry || !RejectedBeforeExecution(err) || attempts.Load() != 1 || deltas.Len() != 0 {
					t.Fatalf("expected one rejected, retryable attempt, got count=%d output=%q err=%v", attempts.Load(), deltas.String(), err)
				}
			}
			result, err := call.Stream(context.Background(), onDelta)
			if err != nil || result.Text != "ok" || result.CompletionTok != 2 || deltas.String() != "ok" || attempts.Load() != 2 {
				t.Fatalf("after fallback: result=%+v output=%q attempts=%d err=%v", result, deltas.String(), attempts.Load(), err)
			}
			native, fallback := <-bodies, <-bodies
			if !bytes.Contains(native, []byte(`"type":"tool_addition"`)) || !bytes.Contains(native, []byte(`"type":"tool_removal"`)) || !bytes.Equal(fallback, wantPlain) {
				t.Fatalf("unexpected catalog or history during fallback: native=%s plain=%s want=%s", native, fallback, wantPlain)
			}
			req.Messages = append(req.Messages, ChatMessage{Role: "assistant", Content: "ok"}, ChatMessage{Role: "user", Content: "next turn"})
			if _, err := client.Stream(context.Background(), req, nil); err != nil {
				t.Fatal(err)
			}
			next := <-bodies
			if bytes.Contains(next, []byte(anthropicToolPlaceholder)) || !bytes.Contains(next, []byte("next turn")) {
				t.Fatalf("next turn did not use ordinary Messages: %s", next)
			}
			req.Model = "another-model"
			if _, err := client.Stream(context.Background(), req, nil); err != nil {
				t.Fatal(err)
			}
			if other := <-bodies; !bytes.Contains(other, []byte(anthropicToolPlaceholder)) || attempts.Load() != 4 {
				t.Fatalf("capability leaked across models: %s", other)
			}
		})
	}
}

func TestAnthropicInlineToolRejectionScope(t *testing.T) {
	client, err := NewClient(Config{Wire: "anthropic", BaseURL: "http://localhost"})
	if err != nil {
		t.Fatal(err)
	}
	req := catalogRequest([]ToolDef{catalogTool("base", "original")}, nil)
	native, err := client.marshalRequestBody(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		status  int
		message string
		want    bool
	}{
		{400, "unknown variant 'tool_removal'", true},
		{422, "tools.1.defer_loading: Extra inputs are not permitted", true},
		{400, "Unexpected value(s) '" + anthropicInlineToolsBeta + "' for the anthropic-beta header", true},
		{400, "messages.4.content.0.tool_addition.tool.definition: invalid input_schema", false},
		{400, "unsupported content type 'thinking'", false},
		{403, "unsupported content type 'tool_addition'", false},
		{500, "unsupported content type 'tool_addition'", false},
	} {
		err := &HTTPRejection{StatusCode: tc.status, Message: tc.message}
		if got := rejectsAnthropicInlineTools(err, native); got != tc.want {
			t.Errorf("native %d %q: fallback=%v want %v", tc.status, tc.message, got, tc.want)
		}
		if rejectsAnthropicInlineTools(err, []byte(`{"model":"m","tools":[]}`)) {
			t.Errorf("already ordinary request retried for %q", tc.message)
		}
	}
}

func TestAnthropicInlineToolErrorAfterOutputDoesNotReplay(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\ndata: "+inlineToolErrorBody+"\n\n")
	}))
	defer server.Close()
	client, err := NewClient(Config{Wire: "anthropic", BaseURL: server.URL, RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	req := catalogRequest([]ToolDef{catalogTool("base", "original")}, nil)
	var deltas strings.Builder
	_, err = client.Stream(context.Background(), req, func(s string) { deltas.WriteString(s) })
	if err == nil || calls.Load() != 1 || deltas.String() != "partial" {
		t.Fatalf("calls=%d output=%q err=%v", calls.Load(), deltas.String(), err)
	}
	body, err := client.marshalRequestBody(req)
	if err != nil || !strings.Contains(anthropicBetaHeader(body), anthropicInlineToolsBeta) {
		t.Fatalf("stream error changed capability: body=%s err=%v", body, err)
	}
}
