package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct{ in, want string }{
		// External bare host → append /v1 (chat and responses share the prefix).
		{"https://cheaprouter.cc", "https://cheaprouter.cc/v1"},
		{"https://gateway.example:8080", "https://gateway.example:8080/v1"},
		{"https://cheaprouter.cc/", "https://cheaprouter.cc/v1"},
		// Idempotent / already concrete → unchanged.
		{"https://cheaprouter.cc/v1", "https://cheaprouter.cc/v1"},
		{"https://cheaprouter.cc/api", "https://cheaprouter.cc/api"},
		{"https://cheaprouter.cc/v1/responses", "https://cheaprouter.cc/v1/responses"},
		{"https://cheaprouter.cc/v1/chat/completions", "https://cheaprouter.cc/v1/chat/completions"},
		// Loopback (local gateways serving at root) → unchanged.
		{"http://localhost:11434", "http://localhost:11434"},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080"},
	}
	for _, c := range cases {
		if got := normalizeBaseURL(c.in); got != c.want {
			t.Errorf("normalizeBaseURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestResponsesThinkingClosesAtFunctionCall verifies the reasoning item is
// finalized when the function call begins, not after its arguments streamed.
func TestResponsesThinkingClosesAtFunctionCall(t *testing.T) {
	var rec sinkRecorder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		write := func(s string) { _, _ = io.WriteString(w, "data: "+s+"\n\n"); w.(http.Flusher).Flush() }
		write(`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}`)
		write(`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"delta":"planning"}`)
		write(`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"read_file"}}`)
		write(`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"file_path\":\"/tmp/a.go\"}"}`)
		write(`{"type":"response.completed","response":{"status":"completed"}}`)
	}))
	defer srv.Close()

	c, err := NewClient(Config{BaseURL: srv.URL, APIKey: "k", Model: "m", Wire: "responses", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.StreamWithReasoning(context.Background(), CompletionRequest{Model: "m"}, rec.delta, rec.reasoning)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(res.ToolCalls))
	}
	want := []string{"thinking:planning", "close"}
	if strings.Join(rec.events, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", rec.events, want)
	}
}

// TestResponsesThinkingClosesOnAnswerDelta verifies answer text closes the
// reasoning phase without a stray empty delta ahead of it.
func TestResponsesThinkingClosesOnAnswerDelta(t *testing.T) {
	var rec sinkRecorder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		write := func(s string) { _, _ = io.WriteString(w, "data: "+s+"\n\n"); w.(http.Flusher).Flush() }
		write(`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"delta":"thinking"}`)
		write(`{"type":"response.output_text.delta","output_index":0,"delta":"the answer"}`)
		write(`{"type":"response.completed","response":{"status":"completed"}}`)
	}))
	defer srv.Close()

	c, err := NewClient(Config{BaseURL: srv.URL, APIKey: "k", Model: "m", Wire: "responses", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.StreamWithReasoning(context.Background(), CompletionRequest{Model: "m"}, rec.delta, rec.reasoning); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	want := []string{"thinking:thinking", "answer:the answer"}
	if strings.Join(rec.events, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", rec.events, want)
	}
}

// TestResponsesWireStreamText drives the Responses wire against a fake SSE
// server and checks that output_text.delta events accumulate into text, that
// the terminal response.completed contributes usage and a finish reason, and
// that the request went to POST /responses.
func TestResponsesWireStreamText(t *testing.T) {
	var gotPath atomic.Value
	var gotBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		if b, err := io.ReadAll(r.Body); err == nil {
			gotBody.Store(string(b))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		write := func(s string) {
			if _, err := w.Write([]byte(s)); err != nil {
				return
			}
			flusher.Flush()
		}
		write("data: " + `{"type":"response.output_text.delta","output_index":0,"delta":"Hello "}` + "\n\n")
		write("data: " + `{"type":"response.output_text.delta","output_index":0,"delta":"world"}` + "\n\n")
		write("data: " + `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"input_tokens_details":{"cached_tokens":3}}}}` + "\n\n")
	}))
	defer srv.Close()

	c, err := NewClient(Config{BaseURL: srv.URL, APIKey: "k", Model: "m", Wire: "responses", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Stream(context.Background(), CompletionRequest{Model: "m", Messages: []ChatMessage{{Role: "user", Content: "hi"}}}, func(string) {})
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if res.Text != "Hello world" {
		t.Fatalf("text = %q, want %q", res.Text, "Hello world")
	}
	if res.PromptTokens != 10 || res.CompletionTok != 5 || res.CachedTokens != 3 || !res.CacheReported {
		t.Fatalf("usage = %+v", res)
	}
	if res.FinishReason != "stop" {
		t.Fatalf("finish reason = %q, want stop", res.FinishReason)
	}
	if p := gotPath.Load(); p != "/responses" {
		t.Fatalf("request path = %v, want /responses", p)
	}
	if b := gotBody.Load().(string); !strings.Contains(b, `"input"`) || !strings.Contains(b, `"stream":true`) {
		t.Fatalf("body = %s", b)
	}
}

// TestResponsesWireFunctionCall verifies a streamed function call is assembled
// from output_item.added + function_call_arguments.delta events.
func TestResponsesWireFunctionCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		write := func(s string) {
			if _, err := w.Write([]byte(s)); err != nil {
				return
			}
			flusher.Flush()
		}
		write("data: " + `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"read_file"}}` + "\n\n")
		write("data: " + `{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"file_path"}` + "\n\n")
		write("data: " + `{"type":"response.function_call_arguments.delta","output_index":0,"delta":"\":\"/tmp/a.go\"}"}` + "\n\n")
		write("data: " + `{"type":"response.completed","response":{"status":"completed"}}` + "\n\n")
	}))
	defer srv.Close()

	c, err := NewClient(Config{BaseURL: srv.URL, Model: "m", Wire: "responses", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Stream(context.Background(), CompletionRequest{Model: "m"}, func(string) {})
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(res.ToolCalls))
	}
	tc := res.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "read_file" {
		t.Fatalf("tool call = %+v", tc)
	}
	if got := tc.Function.Arguments.String(); !strings.Contains(got, "\"file_path\":\"/tmp/a.go\"") {
		t.Fatalf("arguments = %s", got)
	}
}

func TestResponsesFailureNeverReturnsToolCalls(t *testing.T) {
	for _, tc := range []struct{ name, terminal, message string }{
		{"failed", `{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"generation failed"},"usage":{"input_tokens":12,"output_tokens":3}}}`, "generation failed"},
		{"error", `{"type":"error","code":"server_error","message":"stream failed"}`, "stream failed"},
		{"failed_without_details", `{"type":"response.failed","response":{"status":"failed"}}`, "response failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"partial"}`+"\n\n")
				io.WriteString(w, "data: "+`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"c1","name":"Write"}}`+"\n\n")
				io.WriteString(w, "data: "+`{"type":"response.function_call_arguments.done","output_index":1,"arguments":"{}"}`+"\n\n")
				io.WriteString(w, "data: "+tc.terminal+"\n\n")
			}))
			defer srv.Close()
			c, err := NewClient(Config{BaseURL: srv.URL, Wire: "responses", OneAttempt: true})
			if err != nil {
				t.Fatal(err)
			}
			res, err := c.Stream(context.Background(), CompletionRequest{Model: "m"}, nil)
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("error = %v", err)
			}
			if len(res.ToolCalls) != 0 {
				t.Fatalf("failed stream exposed tool calls: %+v", res.ToolCalls)
			}
			if res.Text != "partial" || res.FinishReason != "error" {
				t.Fatalf("result = %+v", res)
			}
			if tc.name == "failed" && (res.PromptTokens != 12 || res.CompletionTok != 3) {
				t.Fatalf("lost usage: %+v", res)
			}
		})
	}
}

func TestResponsesBodyConvertsImageParts(t *testing.T) {
	c, err := NewClient(Config{BaseURL: "http://localhost", Wire: "responses"})
	if err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{"https://example.com/image.png", "data:image/png;base64,AA=="} {
		body, err := c.responsesBody(CompletionRequest{Model: "m", Messages: []ChatMessage{
			{Role: "system", Content: "Be helpful"},
			{Role: "user", Content: []ContentPart{{Type: "text", Text: "Describe"}, {Type: "image_url", ImageURL: &struct {
				URL string `json:"url"`
			}{URL: url}}}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Input []struct {
				Role    string
				Content json.RawMessage
			}
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		if string(got.Input[0].Content) != `"Be helpful"` {
			t.Fatalf("plain text changed: %s", body)
		}
		var parts []map[string]any
		if err := json.Unmarshal(got.Input[1].Content, &parts); err != nil {
			t.Fatal(err)
		}
		if len(parts) != 2 || parts[0]["type"] != "input_text" || parts[0]["text"] != "Describe" || parts[1]["type"] != "input_image" || parts[1]["image_url"] != url {
			t.Fatalf("incorrect parts: %s", body)
		}
	}
}

func TestResponsesPreparedCallConvertsImageParts(t *testing.T) {
	gotBody := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody <- body
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: "+`{"type":"response.completed","response":{"status":"completed"}}`+"\n\n")
	}))
	defer srv.Close()
	c, err := NewClient(Config{BaseURL: srv.URL, Wire: "responses", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	call, err := NewPreparedCall(c, c.Name(), srv.URL, CompletionRequest{Model: "m", Messages: []ChatMessage{{Role: "user", Content: []ContentPart{
		{Type: "text", Text: "Describe"},
		{Type: "image_url", ImageURL: &struct {
			URL string `json:"url"`
		}{URL: "data:image/png;base64,AA=="}},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer call.Close()
	if _, err := call.Stream(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Input []struct {
			Content []struct {
				Type     string
				ImageURL string `json:"image_url"`
			}
		}
	}
	raw := <-gotBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Input) != 1 || len(body.Input[0].Content) != 2 || body.Input[0].Content[0].Type != "input_text" || body.Input[0].Content[1].Type != "input_image" || body.Input[0].Content[1].ImageURL != "data:image/png;base64,AA==" {
		t.Fatalf("prepared image request was not converted: %s", raw)
	}
}
