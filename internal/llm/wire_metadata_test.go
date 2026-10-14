package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPreparedChatExcludesInternalMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		allowed := map[string]bool{"model": true, "messages": true, "tools": true, "stream": true, "stream_options": true, "reasoning_effort": true, "verbosity": true, "thinking": true, "temperature": true, "top_p": true, "max_tokens": true}
		for key := range body {
			if !allowed[key] {
				t.Errorf("unexpected API field %s", key)
				http.Error(w, "unknown parameter", 400)
				return
			}
		}
		if !strings.Contains(string(body["messages"]), "original instructions") {
			t.Error("system instructions missing")
		}
		var messages []struct {
			ReasoningContent string `json:"reasoning_content"`
			Role             string `json:"role"`
			Content          any    `json:"content"`
			ToolCallID       string `json:"tool_call_id"`
			Name             string `json:"name"`
			ToolCalls        []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		}
		decoder := json.NewDecoder(bytes.NewReader(body["messages"]))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&messages); err != nil {
			t.Errorf("invalid nested wire fields: %v", err)
			http.Error(w, "unknown nested parameter", 400)
			return
		}
		if len(messages) != 3 || len(messages[1].ToolCalls) != 1 || messages[1].ToolCalls[0].ID != "call" || messages[1].ToolCalls[0].Function.Arguments != "{}" || messages[2].ToolCallID != "call" {
			t.Errorf("historical tool call/result lost: %+v", messages)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}")
	}))
	defer srv.Close()
	c, err := NewClient(Config{BaseURL: srv.URL, Wire: "chat", OneAttempt: true})
	if err != nil {
		t.Fatal(err)
	}
	req := CompletionRequest{Model: "test", Messages: []ChatMessage{{Role: "system", Content: "original instructions"}, {Role: "assistant", ToolCalls: []ToolCall{{ID: "call", Index: 7, Type: "function", Function: Function{Name: "read", Arguments: "{}"}}}}, {Role: "tool", ToolCallID: "call", Content: "done"}}, SystemBlocks: []SystemBlock{{Text: "original instructions", Cacheable: true}}}
	call, err := NewPreparedCall(c, "test", srv.URL, req)
	if err != nil {
		t.Fatal(err)
	}
	defer call.Close()
	req.SystemBlocks[0].Text = "mutated"
	frozen, err := PreparedRequest(call)
	if err != nil {
		t.Fatal(err)
	}
	if len(frozen.SystemBlocks) != 1 || frozen.SystemBlocks[0].Text != "original instructions" || !frozen.SystemBlocks[0].Cacheable {
		t.Fatalf("lost frozen metadata: %+v", frozen.SystemBlocks)
	}
	if frozen.Messages[1].ToolCalls[0].Index != 7 {
		t.Fatal("prepared tool call metadata was lost")
	}
	if _, err = call.Stream(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestAnthropicCacheBudgetAcrossRequest(t *testing.T) {
	for _, count := range []int{0, 1, 4, 20} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			c, _ := NewClient(Config{BaseURL: "https://example.test", Wire: "anthropic"})
			req := CompletionRequest{Model: "test", Messages: []ChatMessage{{Role: "user", Content: "hello"}, {Role: "assistant", ToolCalls: []ToolCall{{ID: "call", Function: Function{Name: "read", Arguments: "{}"}}}}, {Role: "tool", ToolCallID: "call", Content: "result"}}}
			for i := 0; i < count; i++ {
				req.SystemBlocks = append(req.SystemBlocks, SystemBlock{Text: fmt.Sprint(i), Cacheable: true})
			}
			req.SystemBlocks = append(req.SystemBlocks, SystemBlock{Text: "dynamic"}, SystemBlock{Text: "later stable", Cacheable: true})
			body, err := c.anthropicBody(req)
			if err != nil {
				t.Fatal(err)
			}
			expected := 2
			if count > 0 {
				expected++
			}
			if got := strings.Count(string(body), "cache_control"); got != expected {
				t.Fatalf("markers=%d want=%d body=%s", got, expected, body)
			}
			var decoded struct {
				System []map[string]any `json:"system"`
			}
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatal(err)
			}
			for i, b := range decoded.System {
				_, marked := b["cache_control"]
				if marked != (count > 0 && i == count-1) {
					t.Fatalf("unexpected system breakpoint at %d", i)
				}
			}
		})
	}
}

func TestAnthropicImageFirstMessageDoesNotCreateEmptyCacheText(t *testing.T) {
	c, _ := NewClient(Config{BaseURL: "https://example.test", Wire: "anthropic"})
	req := CompletionRequest{Messages: []ChatMessage{{Role: "user", Content: []map[string]any{{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,YQ=="}}}}}}
	body, err := c.anthropicBody(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "cache_control") || strings.Contains(string(body), `"type":"text"`) {
		t.Fatalf("invented cache text: %s", body)
	}
}
