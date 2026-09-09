package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/llm"
	"ccdp/internal/messages"
)

// newRuntimeAgent builds an agent for runtime-loop tests with its event
// channel drained in the background.
func newRuntimeAgent(t *testing.T) *Agent {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Workspace = dir
	cfg.SessionDir = dir + "/sessions"
	events := make(chan Event, 256)
	ctrl := make(chan Control, 16)
	ag, err := New(&cfg, events, ctrl)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Cleanups run LIFO: Close() first, then close the event channel so no
	// late emit can panic on a closed channel.
	t.Cleanup(func() { close(events) })
	t.Cleanup(ag.Close)
	return ag
}

func TestEstimateTokensUsesRealBaseline(t *testing.T) {
	ag := newRuntimeAgent(t)

	// No provider usage yet: pure estimation.
	full := ag.estimateTokens()

	// Simulate a provider-reported prompt of 10000 tokens anchored at the
	// current history length; a later huge message must push the estimate
	// beyond the anchor.
	ag.mu.Lock()
	ag.history = append(ag.history, messages.Message{Role: messages.RoleUser, Content: "q"})
	anchored := len(ag.history)
	ag.tokenBaseline.promptTokens = 10000
	ag.tokenBaseline.historyLen = anchored
	ag.mu.Unlock()

	if got := ag.estimateTokens(); got < 10000 {
		t.Fatalf("baseline anchor lost: got %d", got)
	}
	ag.mu.Lock()
	ag.history = append(ag.history, messages.Message{Role: messages.RoleUser, Content: strings.Repeat("x", 8000)})
	ag.mu.Unlock()
	if got, want := ag.estimateTokens(), 10000+llm.EstimateTokens(strings.Repeat("x", 8000))+4; got < want-50 || got > want+50 {
		t.Errorf("estimate = %d, want ≈ %d (anchor + incremental)", got, want)
	}

	// A history rewrite (compaction) invalidates the anchor.
	ag.mu.Lock()
	ag.tokenBaseline.promptTokens = 0
	ag.tokenBaseline.historyLen = 0
	ag.mu.Unlock()
	if got := ag.estimateTokens(); got >= 10000 {
		t.Errorf("invalidated baseline should fall back to estimation, got %d", got)
	}
	_ = full
}

func TestInterruptNoteInjectedOnce(t *testing.T) {
	ag := newRuntimeAgent(t)

	ag.mu.Lock()
	ag.interruptNote = true
	ag.mu.Unlock()

	req := ag.buildRequest()
	sys, _ := req.Messages[0].Content.(string)
	if !strings.Contains(sys, "Interrupted turn") {
		t.Error("interrupted-turn guidance missing from system prompt")
	}

	// Second request must not repeat it.
	req = ag.buildRequest()
	sys, _ = req.Messages[0].Content.(string)
	if strings.Contains(sys, "Interrupted turn") {
		t.Error("interrupted-turn guidance should be injected only once")
	}
}

func TestDispatchToolsInterruptedFillsEveryResult(t *testing.T) {
	ag := newRuntimeAgent(t)

	calls := []messages.ToolCall{
		{ID: "c1", Name: "Read", Arguments: map[string]any{}},
		{ID: "c2", Name: "Read", Arguments: map[string]any{}},
		{ID: "c3", Name: "Read", Arguments: map[string]any{}},
	}
	// Simulate an interrupt before dispatch.
	ag.mu.Lock()
	ag.interruptFlag = true
	ag.mu.Unlock()

	results := ag.dispatchTools(calls)
	if len(results) != len(calls) {
		t.Fatalf("expected %d results, got %d", len(calls), len(results))
	}
	// Every result must be filled: an empty output would leave the assistant
	// message with tool_calls that have no matching tool result, which
	// providers reject with 400 on the next request.
	for i, r := range results {
		if r.output == "" {
			t.Errorf("call %d (%s): empty result would break the protocol", i, calls[i].ID)
		}
		if !r.isErr {
			t.Errorf("call %d: interrupted dispatch should be an error result", i)
		}
	}
}

func TestRequestApprovalConcurrentSerialized(t *testing.T) {
	ag := newRuntimeAgent(t)

	// A live turn context so requestApproval's select works.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ag.mu.Lock()
	ag.turnCtx = ctx
	ag.turnCancel = cancel
	ag.mu.Unlock()

	answer := func(ev Event) {
		ag.handle(Control{Type: ControlApproval, ApprovalID: ev.Approval.ID, Approve: true})
	}

	type verdict struct {
		approved bool
	}
	worker := func(tc messages.ToolCall, ch chan verdict) {
		ok, _ := ag.requestApproval(tc, "test")
		ch <- verdict{approved: ok}
	}

	ch1 := make(chan verdict, 1)
	ch2 := make(chan verdict, 1)
	go worker(messages.ToolCall{ID: "t1", Name: "Bash", Arguments: map[string]any{}}, ch1)
	go worker(messages.ToolCall{ID: "t2", Name: "Bash", Arguments: map[string]any{}}, ch2)

	// Two approval modals must appear one after another; each gets answered.
	seen := map[string]bool{}
	deadline := time.After(5 * time.Second)
	for len(seen) < 2 {
		select {
		case ev, ok := <-ag.events:
			if !ok {
				t.Fatal("event channel closed before both approvals surfaced")
			}
			if ev.Type == EventApproval && ev.Approval != nil {
				seen[ev.Approval.ID] = true
				answer(ev)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for both approval modals; seen=%v", seen)
		}
	}
	if !seen["t1"] || !seen["t2"] {
		t.Errorf("both approval requests must surface, got %v", seen)
	}
	select {
	case v := <-ch1:
		if !v.approved {
			t.Error("worker 1 denied, expected approval")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker 1 never finished")
	}
	select {
	case v := <-ch2:
		if !v.approved {
			t.Error("worker 2 denied, expected approval")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker 2 never finished")
	}
}
