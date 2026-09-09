package llm

import (
	"context"
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
