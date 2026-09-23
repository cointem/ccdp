package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// streamSSE writes one SSE data line.
func streamSSE(w http.ResponseWriter, payload string, flush bool) {
	_, _ = w.Write([]byte("data: " + payload + "\n\n"))
	if f, ok := w.(http.Flusher); ok && flush {
		f.Flush()
	}
}

func TestStreamNoRetryAfterDeltasEmitted(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			// First attempt: emit content, then kill the connection mid-stream
			// (retryable scanner error).
			w.Header().Set("Content-Type", "text/event-stream")
			streamSSE(w, `{"choices":[{"delta":{"content":"hello "}}]}`, true)
			panic(http.ErrAbortHandler) // abort the response
		}
		// Second attempt would succeed — but retrying would duplicate text.
		streamSSE(w, `{"choices":[{"delta":{"content":"RETRY-LEAK"}}],"finish_reason":"stop"}`, true)
		streamSSE(w, `[DONE]`, true)
	}))
	defer srv.Close()

	c, err := NewClient(Config{BaseURL: srv.URL, APIKey: "k", Model: "m", RetryDelay: 1})
	if err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	_, err = c.Stream(context.Background(), CompletionRequest{Model: "m"}, func(d string) {
		got.WriteString(d)
	})
	if err == nil {
		t.Fatal("expected error when the stream breaks after emitting deltas")
	}
	if got.String() != "hello " {
		t.Errorf("deltas must not be replayed on retry, got %q", got.String())
	}
}

func TestStreamRetriesBeforeAnyDelta(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			// First attempt fails before any delta (bad gateway).
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		streamSSE(w, `{"choices":[{"delta":{"content":"ok"}}],"finish_reason":"stop"}`, true)
		streamSSE(w, `[DONE]`, true)
	}))
	defer srv.Close()

	c, err := NewClient(Config{BaseURL: srv.URL, APIKey: "k", Model: "m", RetryDelay: 1})
	if err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	res, err := c.Stream(context.Background(), CompletionRequest{Model: "m"}, func(d string) {
		got.WriteString(d)
	})
	if err != nil {
		t.Fatalf("retry after zero deltas should succeed: %v", err)
	}
	if res.Text != "ok" || got.String() != "ok" {
		t.Errorf("unexpected text %q / callbacks %q", res.Text, got.String())
	}
}

func TestStreamCondensesJSONHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{
  "error": {
    "message": "You didn't provide an API key.",
    "type": "invalid_request_error",
    "param": null,
    "code": null
  }
}`))
	}))
	defer srv.Close()

	c, err := NewClient(Config{BaseURL: srv.URL, Model: "m", MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Stream(context.Background(), CompletionRequest{Model: "m"}, nil)
	if err == nil {
		t.Fatal("expected unauthorized error")
	}
	if got := err.Error(); got != "llm: 401 Unauthorized: You didn't provide an API key." {
		t.Fatalf("unexpected error text: %q", got)
	}
}

func TestStreamHonorsRetryAfterHeader(t *testing.T) {
	var attempts atomic.Int32
	start := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			// 429 with Retry-After: 1 — the retry must wait ~1s, not the
			// 50ms configured backoff.
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		streamSSE(w, `{"choices":[{"delta":{"content":"ok"}}],"finish_reason":"stop"}`, true)
		streamSSE(w, `[DONE]`, true)
	}))
	defer srv.Close()

	c, err := NewClient(Config{BaseURL: srv.URL, APIKey: "k", Model: "m", RetryDelay: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Stream(context.Background(), CompletionRequest{Model: "m"}, nil)
	if err != nil {
		t.Fatalf("retry should succeed: %v", err)
	}
	if res.Text != "ok" {
		t.Errorf("unexpected text %q", res.Text)
	}
	waited := time.Since(start)
	if waited < 900*time.Millisecond {
		t.Errorf("Retry-After of 1s should delay the retry ~1s, waited %v", waited)
	}
}

func TestStreamToolCallIndexMerge(t *testing.T) {
	// Two interleaved parallel tool-call streams must merge by index, not by
	// concatenation across calls.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		streamSSE(w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a1","function":{"name":"Read","arguments":"{\"file_"}}]}}]}`, true)
		streamSSE(w, `{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b1","function":{"name":"Write","arguments":"{\"file_"}}]}}]}`, true)
		streamSSE(w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"path\":\"x.go\"}"}}]}}]}`, true)
		streamSSE(w, `{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"path\":\"y.go\",\"content\":\"hi\"}"}}]}}]}`, true)
		streamSSE(w, `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`, true)
		streamSSE(w, `[DONE]`, true)
	}))
	defer srv.Close()

	c, err := NewClient(Config{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Stream(context.Background(), CompletionRequest{Model: "m"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d: %+v", len(res.ToolCalls), res.ToolCalls)
	}
	a, b := res.ToolCalls[0], res.ToolCalls[1]
	if a.ID != "a1" || a.Function.Name != "Read" || a.Function.Arguments.String() != `{"file_path":"x.go"}` {
		t.Errorf("call A corrupted: %+v", a)
	}
	if b.ID != "b1" || b.Function.Name != "Write" || !strings.Contains(b.Function.Arguments.String(), "y.go") {
		t.Errorf("call B corrupted: %+v", b)
	}
}

func TestEstimateTokensWeights(t *testing.T) {
	if got := EstimateTokens(""); got != 0 {
		t.Errorf("empty = %d, want 0", got)
	}
	// ASCII ≈ 4 chars/token: 8000 chars → ~2000 tokens.
	if got := EstimateTokens(strings.Repeat("x", 8000)); got != 2000 {
		t.Errorf("ASCII 8000 chars = %d tokens, want 2000", got)
	}
	// CJK ≈ 1 token/char: 100 3-byte runes → ~100 tokens.
	if got := EstimateTokens(strings.Repeat("中", 100)); got != 100 {
		t.Errorf("CJK 100 chars = %d tokens, want 100", got)
	}
	// 2-byte runes ≈ ½ token/char.
	if got := EstimateTokens(strings.Repeat("é", 100)); got != 50 {
		t.Errorf("2-byte 100 chars = %d tokens, want 50", got)
	}
}

func TestClientCapabilitiesExposeConfiguredAdmissionLimits(t *testing.T) {
	c, err := NewClient(Config{BaseURL: "https://provider.example/v1", Model: "m", ContextWindow: 4096, MaxOutputTokens: 512})
	if err != nil {
		t.Fatal(err)
	}
	caps, ok := ProviderCapabilitiesOf(c)
	if !ok || caps.ContextWindow != 4096 || caps.MaxOutputTokens != 512 || !caps.ToolCalling || !caps.Images || !caps.SystemRole {
		t.Fatalf("client capabilities = %#v, described=%v", caps, ok)
	}
}

func TestNewClientOneAttemptDisablesInternalRetries(t *testing.T) {
	c, err := NewClient(Config{BaseURL: "https://provider.example/v1", Model: "m", OneAttempt: true, MaxRetries: 9})
	if err != nil {
		t.Fatal(err)
	}
	if c.maxRetries != 0 {
		t.Fatalf("one-attempt client maxRetries=%d, want 0", c.maxRetries)
	}
}

func TestRetryAfterPreservesTransientClassification(t *testing.T) {
	base := errors.New("temporary gateway failure")
	err := &RetryableError{Err: base, RetryAfter: 250 * time.Millisecond}
	delay, ok := RetryAfter(fmt.Errorf("request failed: %w", err))
	if !ok || delay != 250*time.Millisecond {
		t.Fatalf("RetryAfter = %s/%v, want 250ms/true", delay, ok)
	}
	if !errors.Is(err, base) {
		t.Fatal("retryable error did not preserve its cause")
	}
}

func TestStreamSurfacesNonStreaming200(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		wantSub     string
	}{
		{"gateway json error", "application/json", `{"error":{"message":"quota exceeded","type":"insufficient_quota"}}`, "quota exceeded"},
		{"provider ignored stream", "application/json", `{"choices":[{"message":{"content":"hi"}}]}`, "non-streaming response"},
		{"empty body", "text/plain", "", "empty"},
		{"opaque body", "text/html", "<html>boom</html>", "unexpected non-streaming response"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c, err := NewClient(Config{BaseURL: srv.URL, APIKey: "k", Model: "m", MaxRetries: 1, RetryDelay: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Stream(context.Background(), CompletionRequest{Model: "m"}, nil)
			if err == nil {
				t.Fatalf("expected an error for a non-streaming 200")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q missing %q", err, tc.wantSub)
			}
		})
	}
}

func TestReasoningStreamRequiresCompletionMarker(t *testing.T) {
	for _, ending := range []string{"", "data: [DONE]\n\n", "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"} {
		t.Run(ending, func(t *testing.T) {
			var attempts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				streamSSE(w, `{"choices":[{"delta":{"reasoning_content":"partial thought"}}]}`, true)
				fmt.Fprint(w, ending)
			}))
			defer srv.Close()
			c, err := NewClient(Config{BaseURL: srv.URL, APIKey: "k", Model: "m", RetryDelay: 1})
			if err != nil {
				t.Fatal(err)
			}
			res, err := c.Stream(context.Background(), CompletionRequest{Model: "m"}, nil)
			if (err != nil) != (ending == "") {
				t.Fatalf("ending=%q error=%v", ending, err)
			}
			if res.Reasoning != "partial thought" || attempts.Load() != 1 {
				t.Fatalf("lost or replayed reasoning: %+v attempts=%d", res, attempts.Load())
			}
		})
	}
}

// sinkRecorder labels what the caller's sinks received, in order, so reasoning
// boundary tests can assert timing (not just that a signal eventually arrived).
type sinkRecorder struct {
	events []string
}

func (s *sinkRecorder) delta(text string) {
	if text == "" {
		s.events = append(s.events, "close")
		return
	}
	s.events = append(s.events, "answer:"+text)
}

func (s *sinkRecorder) reasoning(text string) {
	s.events = append(s.events, "thinking:"+text)
}

// TestChatThinkingClosesAtToolCallBoundary verifies the Chat Completions wire
// finalizes the thinking cell when the model starts emitting a tool call. The
// arguments can stream for a long time, and "Thinking…" must not stay open
// through them.
func TestChatThinkingClosesAtToolCallBoundary(t *testing.T) {
	var rec sinkRecorder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		streamSSE(w, `{"choices":[{"delta":{"reasoning_content":"planning"}}]}`, true)
		streamSSE(w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"write_file","arguments":"{\"a"}}]}}]}`, true)
		streamSSE(w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"}"}}]}}]}`, true)
		streamSSE(w, `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`, true)
		streamSSE(w, `[DONE]`, true)
	}))
	defer srv.Close()

	c, err := NewClient(Config{BaseURL: srv.URL, APIKey: "k", Model: "m", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.StreamWithReasoning(context.Background(), CompletionRequest{Model: "m"}, rec.delta, rec.reasoning); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	want := []string{"thinking:planning", "close"}
	if strings.Join(rec.events, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", rec.events, want)
	}
}

// TestChatThinkingClosesOnAnswerDelta covers the reply that continues into
// answer text: the non-empty delta already finalizes the thinking cell, so the
// wire must not add a stray empty delta in front of it.
func TestChatThinkingClosesOnAnswerDelta(t *testing.T) {
	var rec sinkRecorder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		streamSSE(w, `{"choices":[{"delta":{"reasoning_content":"thinking hard"}}]}`, true)
		streamSSE(w, `{"choices":[{"delta":{"content":"the answer"}}]}`, true)
		streamSSE(w, `[DONE]`, true)
	}))
	defer srv.Close()

	c, err := NewClient(Config{BaseURL: srv.URL, APIKey: "k", Model: "m", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.StreamWithReasoning(context.Background(), CompletionRequest{Model: "m"}, rec.delta, rec.reasoning); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	// The answer delta is itself the close signal, so no empty delta may precede
	// it: a stray one would flicker the turn out of the streaming state.
	want := []string{"thinking:thinking hard", "answer:the answer"}
	if strings.Join(rec.events, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", rec.events, want)
	}
}

// TestChatThinkingClosesOnceForReasoningOnlyReply covers the reply that never
// leaves the thinking phase: finish_reason and the end-of-stream safety net must
// finalize the cell exactly once, not twice.
func TestChatThinkingClosesOnceForReasoningOnlyReply(t *testing.T) {
	var rec sinkRecorder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		streamSSE(w, `{"choices":[{"delta":{"reasoning_content":"plan"}}]}`, true)
		streamSSE(w, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, true)
		streamSSE(w, `[DONE]`, true)
	}))
	defer srv.Close()

	c, err := NewClient(Config{BaseURL: srv.URL, APIKey: "k", Model: "m", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.StreamWithReasoning(context.Background(), CompletionRequest{Model: "m"}, rec.delta, rec.reasoning); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	want := []string{"thinking:plan", "close"}
	if strings.Join(rec.events, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", rec.events, want)
	}
}
