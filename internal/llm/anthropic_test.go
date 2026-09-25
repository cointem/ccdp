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

// TestAnthropicWireStreamText drives the Messages wire against a fake SSE
// server and checks text deltas, usage, finish reason, x-api-key auth, and that
// the request went to POST /messages.
func TestAnthropicWireStreamText(t *testing.T) {
	var gotPath, gotXKey, gotVersion atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		gotXKey.Store(r.Header.Get("x-api-key"))
		gotVersion.Store(r.Header.Get("anthropic-version"))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		write := func(s string) {
			if _, err := w.Write([]byte(s)); err != nil {
				return
			}
			flusher.Flush()
		}
		write("data: " + `{"type":"message_start","message":{"usage":{"input_tokens":12,"output_tokens":0}}}` + "\n\n")
		write("data: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}` + "\n\n")
		write("data: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi "}}` + "\n\n")
		write("data: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"there"}}` + "\n\n")
		write("data: " + `{"type":"content_block_stop","index":0}` + "\n\n")
		write("data: " + `{"type":"message_delta","usage":{"output_tokens":7},"stop_reason":"end_turn"}` + "\n\n")
		write("data: " + `{"type":"message_stop"}` + "\n\n")
	}))
	defer srv.Close()

	c, err := NewClient(Config{BaseURL: srv.URL, APIKey: "k", Model: "m", Wire: "anthropic", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Stream(context.Background(), CompletionRequest{Model: "m", Messages: []ChatMessage{{Role: "user", Content: "hi"}}}, func(string) {})
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if res.Text != "Hi there" {
		t.Fatalf("text = %q, want %q", res.Text, "Hi there")
	}
	if res.PromptTokens != 12 || res.CompletionTok != 7 {
		t.Fatalf("usage = %+v", res)
	}
	if res.FinishReason != "stop" {
		t.Fatalf("finish reason = %q, want stop", res.FinishReason)
	}
	if p := gotPath.Load(); p != "/messages" {
		t.Fatalf("request path = %v, want /messages", p)
	}
	if k := gotXKey.Load(); k != "k" {
		t.Fatalf("x-api-key = %v, want k", k)
	}
	if v := gotVersion.Load(); v != "2023-06-01" {
		t.Fatalf("anthropic-version = %v", v)
	}
}

// TestAnthropicWireThinkingAndToolCall verifies thinking_delta feeds the
// reasoning sink and tool_use blocks are assembled across content_block events.
func TestAnthropicWireThinkingAndToolCall(t *testing.T) {
	var reasoning []string
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
		write("data: " + `{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}` + "\n\n")
		write("data: " + `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me think"}}` + "\n\n")
		write("data: " + `{"type":"content_block_stop","index":0}` + "\n\n")
		write("data: " + `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_1","name":"read_file"}}` + "\n\n")
		write("data: " + `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"file_p"}}` + "\n\n")
		write("data: " + `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"ath\":\"/a.go\"}"}}` + "\n\n")
		write("data: " + `{"type":"content_block_stop","index":1}` + "\n\n")
		write("data: " + `{"type":"message_delta","stop_reason":"tool_use"}` + "\n\n")
		write("data: " + `{"type":"message_stop"}` + "\n\n")
	}))
	defer srv.Close()

	c, err := NewClient(Config{BaseURL: srv.URL, Model: "m", Wire: "anthropic", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.StreamWithReasoning(context.Background(), CompletionRequest{Model: "m"}, func(string) {}, func(s string) {
		reasoning = append(reasoning, s)
	})
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if got := strings.Join(reasoning, ""); got != "Let me think" {
		t.Fatalf("reasoning = %q, want %q", got, "Let me think")
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(res.ToolCalls))
	}
	tc := res.ToolCalls[0]
	if tc.ID != "tu_1" || tc.Function.Name != "read_file" {
		t.Fatalf("tool call = %+v", tc)
	}
	if tc.Function.Arguments.String() != `{"file_path":"/a.go"}` {
		t.Fatalf("tool args = %q", tc.Function.Arguments.String())
	}
	if res.FinishReason != "tool_calls" {
		t.Fatalf("finish reason = %q, want tool_calls", res.FinishReason)
	}
}

// TestAnthropicWireError verifies an anthropic error event surfaces as an error.
func TestAnthropicWireError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("data: " + `{"type":"error","error":{"type":"overloaded_error","message":"server busy"}}` + "\n\n")); err != nil {
			return
		}
	}))
	defer srv.Close()

	c, err := NewClient(Config{BaseURL: srv.URL, Model: "m", Wire: "anthropic", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Stream(context.Background(), CompletionRequest{Model: "m"}, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "server busy") {
		t.Fatalf("expected provider error, got %v", err)
	}
}

// TestAnthropicBody verifies request serialization: system extraction, tool
// mapping, tool_use/tool_result pairing, thinking and image data URLs.
func TestAnthropicBody(t *testing.T) {
	c, err := NewClient(Config{Model: "m", BaseURL: "https://example.com/v1", Wire: "anthropic", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	req := CompletionRequest{
		Model:           "claude",
		ReasoningEffort: "high",
		Messages: []ChatMessage{
			{Role: "system", Content: "you are helpful"},
			{Role: "user", Content: []ContentPart{
				{Type: "text", Text: "look"},
				{Type: "image", ImageURL: &struct {
					URL string `json:"url"`
				}{URL: "data:image/png;base64,aGVsbG8="}},
			}},
			{Role: "assistant", Content: "let me check", ToolCalls: []ToolCall{
				{ID: "tu_9", Function: Function{Name: "read_file", Arguments: ArgumentsJSON(`{"path":"/x"}`)}},
			}},
			{Role: "tool", ToolCallID: "tu_9", Content: "found"},
		},
		Tools: []ToolDef{{Type: "function", Function: FuncDef{Name: "read_file", Description: "Read", Parameters: map[string]any{"type": "object"}}}},
	}
	body, err := c.anthropicBody(req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{
		`"system":"you are helpful"`,
		`"max_tokens":4096`,
		`"thinking":{"type":"adaptive"}`,
		`"output_config":{"effort":"high"}`,
		`"type":"image"`,
		`"media_type":"image/png"`,
		`"type":"tool_use"`,
		`"input":{"path":"/x"}`,
		`"type":"tool_result"`,
		`"tool_use_id":"tu_9"`,
		`"input_schema":{"type":"object"}`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("body missing %s;\n%s", want, s)
		}
	}
	if strings.Contains(s, `"role":"system"`) {
		t.Fatalf("body should not carry a system-role message:\n%s", s)
	}
}

// TestAnthropicWireCacheAndThinkingEnd verifies cache_read tokens are surfaced
// and that closing a thinking block emits an empty text delta so the runtime
// finalizes the reasoning cell before a tool/answer phase begins.
func TestAnthropicWireCacheAndThinkingEnd(t *testing.T) {
	var gotDeltas []string
	var reasoning []string
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
		write("data: " + `{"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":40}}}` + "\n\n")
		write("data: " + `{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}` + "\n\n")
		write("data: " + `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"step one"}}` + "\n\n")
		write("data: " + `{"type":"content_block_stop","index":0}` + "\n\n")
		write("data: " + `{"type":"message_delta","usage":{"output_tokens":9},"stop_reason":"end_turn"}` + "\n\n")
		write("data: " + `{"type":"message_stop"}` + "\n\n")
	}))
	defer srv.Close()

	c, err := NewClient(Config{BaseURL: srv.URL, Model: "m", Wire: "anthropic", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.StreamWithReasoning(context.Background(), CompletionRequest{Model: "m"}, func(s string) {
		gotDeltas = append(gotDeltas, s)
	}, func(s string) { reasoning = append(reasoning, s) })
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	// Anthropic excludes cache reads from input_tokens, so the normalized prompt
	// total is input_tokens + cache_read (100 + 40) and the cached subset is 40.
	if res.PromptTokens != 140 || res.CachedTokens != 40 || !res.CacheReported {
		t.Fatalf("cache/usage = %+v", res)
	}
	if got := strings.Join(reasoning, ""); got != "step one" {
		t.Fatalf("reasoning = %q, want %q", got, "step one")
	}
	// The thinking block stop must have delivered an empty text delta (the
	// runtime's finalize trigger) and never a text fragment.
	foundEmpty := false
	for _, d := range gotDeltas {
		if d == "" {
			foundEmpty = true
		} else {
			t.Fatalf("unexpected text delta %q", d)
		}
	}
	if !foundEmpty {
		t.Fatal("missing empty end-of-thinking delta")
	}
}

// TestAnthropicWireCacheAbsentVersusZero pins the distinction between a provider
// that omits the cache fields (cache presence genuinely unknown) and one that
// sends them explicitly as zero (cache known to be empty). Both must normalize
// input_tokens to the prompt total, but only the explicit zero may mark the
// cache as reported — otherwise the footer falls back to "cache ?".
func TestAnthropicWireCacheAbsentVersusZero(t *testing.T) {
	cases := []struct {
		name     string
		usage    string
		prompt   int
		cached   int
		reported bool
	}{
		{name: "explicit zero", usage: `{"input_tokens":64,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`, prompt: 64, cached: 0, reported: true},
		{name: "cache write only", usage: `{"input_tokens":64,"cache_read_input_tokens":0,"cache_creation_input_tokens":2560}`, prompt: 2624, cached: 0, reported: true},
		{name: "fields absent", usage: `{"input_tokens":64}`, prompt: 64, cached: 0, reported: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
				write("data: " + `{"type":"message_start","message":{"usage":` + tc.usage + `}}` + "\n\n")
				write("data: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}` + "\n\n")
				write("data: " + `{"type":"message_delta","usage":{"output_tokens":7},"stop_reason":"end_turn"}` + "\n\n")
				write("data: " + `{"type":"message_stop"}` + "\n\n")
			}))
			defer srv.Close()

			c, err := NewClient(Config{BaseURL: srv.URL, Model: "m", Wire: "anthropic", Timeout: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			res, err := c.StreamWithReasoning(context.Background(), CompletionRequest{Model: "m"}, nil, nil)
			if err != nil {
				t.Fatalf("stream error: %v", err)
			}
			if res.PromptTokens != tc.prompt || res.CachedTokens != tc.cached || res.CacheReported != tc.reported {
				t.Fatalf("usage = %+v, want prompt=%d cached=%d reported=%v", res, tc.prompt, tc.cached, tc.reported)
			}
			// message_delta carries output_tokens only; it must not zero the
			// prompt totals established at message_start.
			if res.PromptTokens == 0 {
				t.Fatal("message_delta zeroed the prompt total")
			}
		})
	}
}

// TestAnthropicBodySystemBlocks verifies that structured SystemBlocks render as
// a system array with cache_control on cacheable sections, and that backbone
// cache_control lands on the first message and the trailing tool_result.
func TestAnthropicBodySystemBlocks(t *testing.T) {
	c, err := NewClient(Config{Model: "m", BaseURL: "https://example.com/v1", Wire: "anthropic", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	req := CompletionRequest{
		Model: "claude",
		SystemBlocks: []SystemBlock{
			{Text: "CORE", Cacheable: true},
			{Text: "# Project instructions\nRULES", Cacheable: true},
			{Text: "# Environment", Cacheable: false},
		},
		Messages: []ChatMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: ""},
			{Role: "tool", ToolCallID: "t1", Content: "result"},
		},
	}
	body, err := c.anthropicBody(req)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}

	sys, ok := doc["system"].([]any)
	if !ok || len(sys) != 3 {
		t.Fatalf("system should be a 3-element array, got %v", doc["system"])
	}
	hasCC := func(m map[string]any) bool {
		_, ok := m["cache_control"]
		return ok
	}
	if !hasCC(sys[0].(map[string]any)) || !hasCC(sys[1].(map[string]any)) {
		t.Fatalf("cacheable system blocks must carry cache_control: %v", sys)
	}
	if hasCC(sys[2].(map[string]any)) {
		t.Fatalf("dynamic system block must not be cached: %v", sys[2])
	}

	msgs := doc["messages"].([]any)
	first := msgs[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if !hasCC(first) {
		t.Fatalf("first message backbone must carry cache_control: %v", first)
	}
	toolRes := msgs[len(msgs)-1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if toolRes["type"] != "tool_result" || !hasCC(toolRes) {
		t.Fatalf("trailing tool_result backbone must carry cache_control: %v", toolRes)
	}
}

// TestAnthropicBodySystemFallback verifies that without SystemBlocks the system
// is still a plain string extracted from system-role messages (legacy path).
func TestAnthropicBodySystemFallback(t *testing.T) {
	c, err := NewClient(Config{Model: "m", BaseURL: "https://example.com/v1", Wire: "anthropic", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	req := CompletionRequest{Model: "claude", Messages: []ChatMessage{
		{Role: "system", Content: "LEGACY_SYS"},
		{Role: "user", Content: "hi"},
	}}
	body, err := c.anthropicBody(req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, `"system":"LEGACY_SYS"`) {
		t.Fatalf("fallback system string expected:\n%s", s)
	}
}

// TestAnthropicBodyAdaptiveThinking covers the effort folding and the beta
// headers: ccdp-only levels map into low|medium|high|max, "none" disables the
// thinking block entirely, and thinking without effort stays adaptive-only.
func TestAnthropicBodyAdaptiveThinking(t *testing.T) {
	c, err := NewClient(Config{Model: "m", BaseURL: "https://example.com/v1", Wire: "anthropic", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, effort string
		reqThinking  string
		wantThinking string
		wantOutput   string
		wantBetas    string
	}{
		{name: "xhigh folds to high", effort: "xhigh", wantThinking: `"thinking":{"type":"adaptive"}`, wantOutput: `"output_config":{"effort":"high"}`, wantBetas: anthropicInterleavedThinkingBeta + ", " + anthropicEffortBeta},
		{name: "max passes through", effort: "max", wantThinking: `"thinking":{"type":"adaptive"}`, wantOutput: `"output_config":{"effort":"max"}`, wantBetas: anthropicInterleavedThinkingBeta + ", " + anthropicEffortBeta},
		{name: "none disables thinking", effort: "none"},
		{name: "unknown effort falls back", effort: "banana"},
		{name: "thinking without effort", reqThinking: "enabled", wantThinking: `"thinking":{"type":"adaptive"}`, wantBetas: anthropicInterleavedThinkingBeta},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := CompletionRequest{Model: "claude", ReasoningEffort: tc.effort, Messages: []ChatMessage{{Role: "user", Content: "hi"}}}
			if tc.reqThinking != "" {
				req.Thinking = &ThinkingConfig{Type: tc.reqThinking}
			}
			body, err := c.anthropicBody(req)
			if err != nil {
				t.Fatal(err)
			}
			s := string(body)
			if tc.wantThinking != "" && !strings.Contains(s, tc.wantThinking) {
				t.Fatalf("body missing %s:\n%s", tc.wantThinking, s)
			}
			if tc.wantOutput != "" && !strings.Contains(s, tc.wantOutput) {
				t.Fatalf("body missing %s:\n%s", tc.wantOutput, s)
			}
			if tc.wantThinking == "" && strings.Contains(s, `"thinking"`) {
				t.Fatalf("thinking block must be absent:\n%s", s)
			}
			if got := anthropicBetaHeader(body); got != tc.wantBetas {
				t.Fatalf("anthropic-beta = %q, want %q", got, tc.wantBetas)
			}
		})
	}
}

// TestAnthropicWireRedactedThinkingEnd verifies a redacted_thinking block also
// finalizes reasoning (previously only literal "thinking" matched, so redacted
// thinking kept the UI thinking state active until the whole reply finished).
func TestAnthropicWireRedactedThinkingEnd(t *testing.T) {
	var gotDeltas []string
	var reasoning []string
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
		write("data: " + `{"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking"}}` + "\n\n")
		write("data: " + `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hidden chain"}}` + "\n\n")
		write("data: " + `{"type":"content_block_stop","index":0}` + "\n\n")
		write("data: " + `{"type":"content_block_start","index":1,"content_block":{"type":"text"}}` + "\n\n")
		write("data: " + `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}` + "\n\n")
		write("data: " + `{"type":"message_stop"}` + "\n\n")
	}))
	defer srv.Close()

	c, err := NewClient(Config{BaseURL: srv.URL, Model: "m", Wire: "anthropic", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.StreamWithReasoning(context.Background(), CompletionRequest{Model: "m"}, func(s string) {
		gotDeltas = append(gotDeltas, s)
	}, func(s string) { reasoning = append(reasoning, s) })
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if got := strings.Join(reasoning, ""); got != "hidden chain" {
		t.Fatalf("reasoning = %q, want %q", got, "hidden chain")
	}
	if res.Text != "answer" {
		t.Fatalf("text = %q, want %q", res.Text, "answer")
	}
	// An empty text delta must have been emitted at the thinking boundary so the
	// runtime finalizes the reasoning cell.
	foundEmpty, sawText := false, false
	for _, d := range gotDeltas {
		if d == "" {
			foundEmpty = true
		} else if d == "answer" {
			sawText = true
		}
	}
	if !foundEmpty {
		t.Fatalf("missing empty end-of-thinking delta for redacted_thinking")
	}
	if !sawText {
		t.Fatalf("answer text not delivered")
	}
}

// TestParseDataURL verifies media type / base64 extraction.
func TestParseDataURL(t *testing.T) {
	m, d, err := parseDataURL("data:image/jpeg;base64,YWJj")
	if err != nil {
		t.Fatal(err)
	}
	if m != "image/jpeg" || d != "YWJj" {
		t.Fatalf("got (%q,%q)", m, d)
	}
	if _, _, err := parseDataURL("https://example.com/a.png"); err == nil {
		t.Fatal("expected error for non-data URL")
	}
	if _, _, err := parseDataURL("data:image/png;base64,===="); err != nil {
		t.Fatalf("unexpected error normalizing base64: %v", err)
	}
}

// TestAnthropicBodyRead verifies the SSE streamed body from an httptest server
// is consumed as the wire-specific payload (integration-ish via Stream).
func TestAnthropicBodyRead(t *testing.T) {
	_ = io.Discard // keep io imported consistent with sibling tests
	// Light coverage only; full request-body assertions live in TestAnthropicBody.
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: " + `{"type":"message_stop"}` + "\n\n"))
	}))
	defer srv.Close()
	c, err := NewClient(Config{BaseURL: srv.URL, Model: "m", Wire: "anthropic", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Stream(context.Background(), CompletionRequest{Model: "m", Messages: []ChatMessage{{Role: "user", Content: "hi"}}}, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"stream":true`) {
		t.Fatalf("stream flag missing: %s", gotBody)
	}
}
