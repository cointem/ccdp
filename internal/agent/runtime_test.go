package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/tools"
)

// truncateForLog shortens a message for test failure dumps.
func truncateForLog(s string) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}

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

	// Two DIFFERENT invocations: each gets its own modal, one after another
	// (the approvalMu slot must never overlap modals).
	ch1 := make(chan verdict, 1)
	ch2 := make(chan verdict, 1)
	go worker(messages.ToolCall{ID: "t1", Name: "Bash", Arguments: map[string]any{"command": "make build"}}, ch1)
	go worker(messages.ToolCall{ID: "t2", Name: "Bash", Arguments: map[string]any{"command": "make test"}}, ch2)

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

func TestRequestApprovalCacheReplaysDecision(t *testing.T) {
	ag := newRuntimeAgent(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ag.mu.Lock()
	ag.turnCtx = ctx
	ag.turnCancel = cancel
	ag.mu.Unlock()

	// Two identical parallel invocations: only ONE modal may appear; the
	// second worker replays the cached decision (Codex's approval cache).
	type verdict struct {
		approved bool
	}
	tc := messages.ToolCall{ID: "t1", Name: "Bash", Arguments: map[string]any{"command": "make build"}}
	ch1 := make(chan verdict, 1)
	ch2 := make(chan verdict, 1)
	go func() { ok, _ := ag.requestApproval(tc, "test"); ch1 <- verdict{ok} }()
	go func() { ok, _ := ag.requestApproval(tc, "test"); ch2 <- verdict{ok} }()

	modals := 0
	deadline := time.After(5 * time.Second)
	for modals < 1 {
		select {
		case ev, ok := <-ag.events:
			if !ok {
				t.Fatal("event channel closed before the approval surfaced")
			}
			if ev.Type == EventApproval && ev.Approval != nil {
				modals++
				ag.handle(Control{Type: ControlApproval, ApprovalID: ev.Approval.ID, Approve: true})
			}
		case <-deadline:
			t.Fatal("timed out waiting for the approval modal")
		}
	}
	// Drain remaining events briefly; no second modal may arrive.
drained:
	for {
		select {
		case ev, ok := <-ag.events:
			if !ok {
				break drained
			}
			if ev.Type == EventApproval && ev.Approval != nil {
				t.Fatalf("second approval modal surfaced for an identical call: %s", ev.Approval.ID)
			}
		case <-time.After(300 * time.Millisecond):
			break drained
		}
	}
	for _, ch := range []chan verdict{ch1, ch2} {
		select {
		case v := <-ch:
			if !v.approved {
				t.Error("worker denied, expected the cached approval")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("worker never finished")
		}
	}
}

// TestTruncatedToolCallsVoided pins pi's failToolCallsFromTruncatedMessage
// behavior: tool calls carried by a length-truncated reply are never executed;
// they get error results so the model can re-issue them.
func TestTruncatedToolCallsVoided(t *testing.T) {
	f := &fakeLLM{script: []string{
		"truncated:Bash|command=echo should-not-run",
		"text:done",
	}}
	ag, events, ctrl := newTestAgent(t, f)
	go ag.Run()

	ctrl <- Control{Type: ControlUserMessage, Text: "run it"}

	saw := drainUntil(t, events, 10*time.Second, func(ev Event) bool {
		return ev.Type == EventToolResult && ev.Tool != nil && ev.Tool.Status == "denied"
	})
	found := false
	for _, ev := range saw {
		if ev.Tool != nil && ev.Tool.Name == "Bash" && strings.Contains(ev.Tool.Output, "truncated") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a voided (truncated) tool event, got %+v", saw)
	}

	// The tool result in history must be an error telling the model to
	// re-issue, and the command must not have executed.
	ag.mu.Lock()
	var toolMsg string
	for i := len(ag.history) - 1; i >= 0; i-- {
		if ag.history[i].Role == messages.RoleTool {
			toolMsg = ag.history[i].Content
			break
		}
	}
	ag.mu.Unlock()
	if !strings.Contains(toolMsg, "not executed") || !strings.Contains(toolMsg, "Re-issue") {
		t.Fatalf("expected re-issue error tool result, got %q", toolMsg)
	}
	if strings.Contains(toolMsg, "should-not-run") {
		t.Fatal("truncated tool call was executed; it must be voided")
	}
}

// TestSteerQueuedMessageInjected pins the steering semantics: a message sent
// while the agent is busy is injected into the RUNNING turn at the next
// tool-result boundary, not deferred to a follow-up turn.
func TestSteerQueuedMessageInjected(t *testing.T) {
	f := &fakeLLM{script: []string{
		"tool:Bash|command=sleep 0.4",
		"text:done",
	}}
	ag, events, ctrl := newTestAgent(t, f)
	go ag.Run()

	ctrl <- Control{Type: ControlUserMessage, Text: "first message"}
	time.Sleep(100 * time.Millisecond) // the turn is now inside the Bash call
	ctrl <- Control{Type: ControlUserMessage, Text: "steered message"}

	drainUntil(t, events, 10*time.Second, func(ev Event) bool {
		return ev.Type == EventTurnDone
	})

	ag.mu.Lock()
	defer ag.mu.Unlock()
	// Expected shape: [user1, asst(tool), tool, user2(steered), asst(done)]
	// (the system prompt lives outside history).
	var dump strings.Builder
	for i, m := range ag.history {
		fmt.Fprintf(&dump, "%d:%s:%q ", i, m.Role, truncateForLog(m.Content))
	}
	t.Logf("history: %s", dump.String())
	n := len(ag.history)
	if n != 5 {
		t.Fatalf("steered history should be 5 messages, got %d", n)
	}
	last := ag.history[n-1]
	prev := ag.history[n-2]
	if last.Role != messages.RoleAssistant || !strings.Contains(last.Content, "done") {
		t.Fatalf("expected final assistant 'done', got %q (%s)", last.Content, last.Role)
	}
	if prev.Role != messages.RoleUser || prev.Content != "steered message" {
		t.Fatalf("steered message must be the last user message before the reply, got %q (%s)", prev.Content, prev.Role)
	}
}

// TestForkBranchesSession pins the session-branch behavior: the new session
// carries history[:keep] plus a branch note, records lineage, and leaves the
// source session file untouched.
func TestForkBranchesSession(t *testing.T) {
	f := &fakeLLM{script: []string{
		"tool:Bash|command=echo one",
		"text:final answer for the first turn",
		"text:a branch summary of the abandoned direction",
	}}
	ag, events, ctrl := newTestAgent(t, f)
	go ag.Run()

	ctrl <- Control{Type: ControlUserMessage, Text: "try direction A"}
	drainUntil(t, events, 10*time.Second, func(ev Event) bool {
		return ev.Type == EventTurnDone
	})

	oldID := ag.SessionID()
	// turnFinished (which persists the session) runs after the TurnDone event
	// is emitted; poll briefly for the source snapshot to land on disk.
	var oldSnap *SessionSnapshot
	var err error
	for i := 0; i < 40; i++ {
		if oldSnap, err = LoadSession(ag.SessionDir(), oldID); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("source session not saved: %v", err)
	}
	oldCount := len(oldSnap.History)

	ctrl <- Control{Type: ControlFork, Count: 1} // keep [user1], drop the tool round
	drainUntil(t, events, 15*time.Second, func(ev Event) bool {
		return ev.Type == EventSessionChanged
	})

	newID := ag.SessionID()
	if newID == oldID {
		t.Fatal("fork did not switch the session id")
	}
	if parent, point := ag.Lineage(); parent != oldID || point != 1 {
		t.Fatalf("lineage = (%q, %d), want (%q, 1)", parent, point, oldID)
	}

	ag.mu.Lock()
	hist := append([]messages.Message(nil), ag.history...)
	ag.mu.Unlock()
	if len(hist) != 2 { // branch-note + user1
		t.Fatalf("branched history should be note+user (2), got %d: %v", len(hist), hist)
	}
	if hist[0].Role != messages.RoleSystem || !strings.Contains(hist[0].Content, "branched from session "+oldID) {
		t.Fatalf("expected branch note first, got %q", hist[0].Content)
	}
	if !strings.Contains(hist[0].Content, "branch summary") {
		t.Fatalf("expected the abandoned-direction summary in the note, got %q", hist[0].Content)
	}

	snap, err := LoadSession(ag.SessionDir(), newID)
	if err != nil {
		t.Fatalf("branched session not saved: %v", err)
	}
	if snap.ParentID != oldID || snap.BranchPoint != 1 {
		t.Fatalf("snapshot lineage = (%q, %d)", snap.ParentID, snap.BranchPoint)
	}
	// The source session file keeps its full history.
	after, err := LoadSession(ag.SessionDir(), oldID)
	if err != nil {
		t.Fatalf("source session missing after fork: %v", err)
	}
	if len(after.History) != oldCount {
		t.Fatalf("source session mutated by fork: %d → %d", oldCount, len(after.History))
	}
}

// TestPostCompactFileAttachments pins the post-compaction file restore: files
// read earlier are re-attached to the summary, files still referenced by the
// kept tail are skipped, and missing files are dropped.
func TestPostCompactFileAttachments(t *testing.T) {
	ag := newRuntimeAgent(t)
	dir := t.TempDir()
	old := strings.Repeat("old file content\n", 10)
	fresh := "fresh file content"
	if err := os.WriteFile(dir+"/old.go", []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/fresh.go", []byte(fresh), 0o644); err != nil {
		t.Fatal(err)
	}
	tools.MarkFileRead(dir + "/old.go")
	tools.MarkFileRead(dir + "/fresh.go")
	t.Cleanup(tools.ClearFileReadState)

	// fresh.go is still referenced by the kept tail → skipped; old.go attached.
	tail := []messages.Message{{
		Role: messages.RoleAssistant,
		ToolCalls: []messages.ToolCall{{
			ID: "c1", Name: "Read", Arguments: map[string]any{"file_path": dir + "/fresh.go"},
		}},
	}}
	out := ag.postCompactFileAttachments(tail)
	if !strings.Contains(out, "old file content") {
		t.Fatalf("old.go content missing from attachments:\n%s", out)
	}
	if strings.Contains(out, "fresh file content") {
		t.Fatal("kept-tail file should not be re-attached")
	}

	// A file that disappeared is dropped silently.
	if err := os.Remove(dir + "/old.go"); err != nil {
		t.Fatal(err)
	}
	if out := ag.postCompactFileAttachments(nil); strings.Contains(out, "old file content") {
		t.Fatal("deleted file should not be attached")
	}
}

// TestPendingInboxPersisted pins the persistent inbox: queued user messages
// ride in the session snapshot and are restored on resume.
func TestPendingInboxPersisted(t *testing.T) {
	ag := newRuntimeAgent(t)

	// Queue messages as if the user typed them while a turn was running.
	ag.mu.Lock()
	ag.pendingMsgs = []string{"queued one", "queued two"}
	ag.mu.Unlock()

	if err := ag.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	snap, err := LoadSession(ag.SessionDir(), ag.SessionID())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(snap.Pending) != 2 || snap.Pending[0] != "queued one" {
		t.Fatalf("snapshot pending = %v", snap.Pending)
	}

	// Resume restores the inbox.
	snap.Pending = append(snap.Pending, "queued three")
	resumed, err := Resume(ag.cfg, snap, ag.events, ag.ctrl)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	defer resumed.Close()
	resumed.mu.Lock()
	got := append([]string(nil), resumed.pendingMsgs...)
	resumed.mu.Unlock()
	if len(got) != 3 || got[2] != "queued three" {
		t.Fatalf("resumed pending inbox = %v", got)
	}
}
