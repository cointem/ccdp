package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/permissions"
)

// fakeLLM is a scripted OpenAI-compatible streaming server. Each turn it emits
// either a text-only response or a tool call, based on the script.
type fakeLLM struct {
	mu       sync.Mutex
	calls    int
	script   []string // per-request behavior: "text:..." or "tool:Bash|cmd:..."
	requests []map[string]any
}

func (f *fakeLLM) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	idx := f.calls
	f.calls++
	f.mu.Unlock()

	var req map[string]any
	body := make([]byte, 0)
	buf := new(strings.Builder)
	// Read body
	tmp := make([]byte, 64*1024)
	for {
		n, err := r.Body.Read(tmp)
		buf.Write(tmp[:n])
		if err != nil {
			break
		}
	}
	body = []byte(buf.String())
	_ = json.Unmarshal(body, &req)

	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()

	step := "text:ok"
	if idx < len(f.script) {
		step = f.script[idx]
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)

	fl, _ := w.(http.Flusher)

	if strings.HasPrefix(step, "text:") {
		text := strings.TrimPrefix(step, "text:")
		writeChunk(w, fl, `{"id":"1","object":"chat.completion.chunk","choices":[{"delta":{"role":"assistant","content":"`+jsonEscape(text)+`"},"finish_reason":null}]}`)
		writeChunk(w, fl, `{"id":"1","object":"chat.completion.chunk","choices":[{"delta":{},"finish_reason":"stop"}]}`)
		writeChunk(w, fl, "[DONE]")
		return
	}

	if strings.HasPrefix(step, "tool:") {
		// tool:Name|arg=value;arg2=value2|thought
		parts := strings.SplitN(step, "|", 3)
		toolName := strings.TrimPrefix(parts[0], "tool:")
		args := parts[1]
		thought := ""
		if len(parts) > 2 {
			thought = parts[2]
		}
		argsJSON := buildArgsJSON(args)
		writeChunk(w, fl, `{"id":"1","object":"chat.completion.chunk","choices":[{"delta":{"role":"assistant","content":"`+jsonEscape(thought)+`"},"finish_reason":null}]}`)
		writeChunk(w, fl, `{"id":"1","object":"chat.completion.chunk","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"`+toolName+`","arguments":`+jsonString(argsJSON)+`}}]},"finish_reason":null}]}`)
		writeChunk(w, fl, `{"id":"1","object":"chat.completion.chunk","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
		writeChunk(w, fl, "[DONE]")
		return
	}

	if strings.HasPrefix(step, "truncated:") {
		// A tool call cut off by the token limit: the call streams fine but
		// the reply ends with finish_reason "length" (pi's truncated-message
		// case — the arguments may be incomplete even when they parse).
		parts := strings.SplitN(step, "|", 2)
		toolName := strings.TrimPrefix(parts[0], "truncated:")
		args := ""
		if len(parts) > 1 {
			args = parts[1]
		}
		argsJSON := buildArgsJSON(args)
		writeChunk(w, fl, `{"id":"1","object":"chat.completion.chunk","choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`)
		writeChunk(w, fl, `{"id":"1","object":"chat.completion.chunk","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"`+toolName+`","arguments":`+jsonString(argsJSON)+`}}]},"finish_reason":null}]}`)
		writeChunk(w, fl, `{"id":"1","object":"chat.completion.chunk","choices":[{"delta":{},"finish_reason":"length"}]}`)
		writeChunk(w, fl, "[DONE]")
		return
	}

	writeChunk(w, fl, `{"id":"1","object":"chat.completion.chunk","choices":[{"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`)
	writeChunk(w, fl, `{"id":"1","object":"chat.completion.chunk","choices":[{"delta":{},"finish_reason":"stop"}]}`)
	writeChunk(w, fl, "[DONE]")
}

func writeChunk(w http.ResponseWriter, fl http.Flusher, data string) {
	fmt.Fprintf(w, "data: %s\n\n", data)
	if fl != nil {
		fl.Flush()
	}
}

// jsonString returns the JSON-quoted string form of s.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return strings.Trim(string(b), `"`)
}

// buildArgsJSON converts "cmd=ls -la;path=x y" into a JSON object string.
func buildArgsJSON(s string) string {
	m := map[string]any{}
	if s != "" {
		for _, kv := range strings.Split(s, ";") {
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) == 2 {
				m[parts[0]] = parts[1]
			}
		}
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func newTestAgent(t *testing.T, f *fakeLLM) (*Agent, <-chan Event, chan Control) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)

	cfg := config.Default()
	cfg.BaseURL = srv.URL
	cfg.Model = "fake-model"
	cfg.Workspace = t.TempDir()
	cfg.APIKey = "test"
	cfg.PermissionMode = string(permissions.ModeBypass)
	cfg.SessionDir = t.TempDir()
	cfg.SystemPrompt = "test system prompt"

	events := make(chan Event, 4096)
	ctrl := make(chan Control, 64)
	ag, err := New(&cfg, events, ctrl)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ag, events, ctrl
}

// drainEvents consumes events until a predicate matches or a timeout passes.
func drainUntil(t *testing.T, events <-chan Event, timeout time.Duration, pred func(Event) bool) []Event {
	t.Helper()
	var seen []Event
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-events:
			seen = append(seen, ev)
			if pred(ev) {
				return seen
			}
		case <-deadline:
			t.Fatalf("timeout waiting for event; saw %d events", len(seen))
		}
	}
}

func TestTurnWithToolCall(t *testing.T) {
	f := &fakeLLM{script: []string{
		"tool:Bash|command=echo hello",
		"text:done",
	}}
	ag, events, ctrl := newTestAgent(t, f)
	go ag.Run()

	ctrl <- Control{Type: ControlUserMessage, Text: "list the directory"}

	// Expect a tool result carrying the bash output.
	saw := drainUntil(t, events, 10*time.Second, func(ev Event) bool {
		return ev.Type == EventToolResult && ev.Tool != nil && ev.Tool.Status == "success"
	})
	found := false
	for _, ev := range saw {
		if ev.Type == EventToolResult && strings.Contains(ev.Tool.Output, "hello") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected bash output 'hello' in tool result events, got %+v", saw)
	}

	// Turn should end; the final assistant streamed text must be "done".
	finished := drainUntil(t, events, 10*time.Second, func(ev Event) bool {
		return ev.Type == EventTurnDone
	})
	foundDone := false
	for _, ev := range finished {
		if ev.Type == EventStream && strings.Contains(ev.Text, "done") {
			foundDone = true
		}
	}
	if !foundDone {
		t.Fatalf("expected final assistant text 'done', got %+v", finished)
	}
}

func TestPermissionDenied(t *testing.T) {
	f := &fakeLLM{script: []string{
		"tool:Bash|command=rm -rf /",
	}}
	ag, events, ctrl := newTestAgent(t, f)
	// Force default mode so the dangerous command is denied.
	ag.SetPermissionMode(permissions.ModeDefault)
	go ag.Run()

	ctrl <- Control{Type: ControlUserMessage, Text: "delete root"}

	saw := drainUntil(t, events, 10*time.Second, func(ev Event) bool {
		return ev.Type == EventToolResult && ev.Tool != nil && ev.Tool.Status == "denied"
	})
	if len(saw) == 0 {
		t.Fatalf("expected denied tool event for dangerous command")
	}

	// The history should contain a permission-denied tool result.
	ag.mu.Lock()
	last := ag.history[len(ag.history)-1]
	ag.mu.Unlock()
	if last.Role != "tool" || !strings.Contains(last.Content, "permission denied") {
		t.Fatalf("expected permission denied tool result, got %+v", last)
	}
}

func TestApprovalGate(t *testing.T) {
	f := &fakeLLM{script: []string{
		"tool:Bash|command=npm install",
		"text:installed",
	}}
	ag, events, ctrl := newTestAgent(t, f)
	ag.SetPermissionMode(permissions.ModeDefault)
	go ag.Run()

	ctrl <- Control{Type: ControlUserMessage, Text: "install deps"}

	// Approval should be requested (npm install is not in the safe allowlist).
	req := drainUntil(t, events, 10*time.Second, func(ev Event) bool {
		return ev.Type == EventApproval
	})
	ap := req[len(req)-1].Approval
	if ap == nil {
		t.Fatalf("expected approval request")
	}

	// Approve it (not remembered).
	ctrl <- Control{Type: ControlApproval, ApprovalID: ap.ID, Approve: true, Remember: false}

	// Tool should then run successfully.
	saw := drainUntil(t, events, 10*time.Second, func(ev Event) bool {
		return ev.Type == EventToolResult && ev.Tool != nil && ev.Tool.Status == "success"
	})
	if len(saw) == 0 {
		t.Fatalf("expected tool to run after approval")
	}
}

func TestApprovalDeniedRemembered(t *testing.T) {
	f := &fakeLLM{script: []string{
		"tool:Bash|command=npm install",
		"text:done",
	}}
	ag, events, ctrl := newTestAgent(t, f)
	ag.SetPermissionMode(permissions.ModeDefault)
	go ag.Run()

	ctrl <- Control{Type: ControlUserMessage, Text: "install deps"}

	req := drainUntil(t, events, 10*time.Second, func(ev Event) bool {
		return ev.Type == EventApproval
	})
	ap := req[len(req)-1].Approval

	// Deny and remember ("never allow this command").
	ctrl <- Control{Type: ControlApproval, ApprovalID: ap.ID, Approve: false, Remember: true}

	saw := drainUntil(t, events, 10*time.Second, func(ev Event) bool {
		return ev.Type == EventToolResult && ev.Tool != nil && ev.Tool.Status == "denied"
	})
	if len(saw) == 0 {
		t.Fatalf("expected denied tool event")
	}

	// Let the first turn finish completely (it consumes script index 1).
	drainUntil(t, events, 10*time.Second, func(ev Event) bool {
		return ev.Type == EventTurnDone
	})

	// Second identical request must be auto-denied without asking.
	f.mu.Lock()
	f.calls = 0
	f.script = []string{"tool:Bash|command=npm install", "text:done"}
	f.mu.Unlock()

	ctrl <- Control{Type: ControlUserMessage, Text: "install deps again"}
	// No approval event should arrive; the tool is denied immediately.
	deadline := time.After(3 * time.Second)
	gotApproval := false
	gotDenied := false
	for !gotDenied {
		select {
		case ev := <-events:
			if ev.Type == EventApproval {
				gotApproval = true
			}
			if ev.Type == EventToolResult && ev.Tool != nil && ev.Tool.Status == "denied" {
				gotDenied = true
			}
		case <-deadline:
			t.Fatalf("timeout; approval=%v denied=%v", gotApproval, gotDenied)
		}
	}
	if gotApproval {
		t.Fatalf("expected auto-denial without approval prompt, but approval was requested")
	}
}
