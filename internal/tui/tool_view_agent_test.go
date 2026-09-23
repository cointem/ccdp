package tui

import (
	"strings"
	"testing"
	"time"

	"ccdp/internal/protocol"
)

func agentTaskItem(status string, args map[string]any, output string, children ...protocol.ChildSession) historyCell {
	return historyCell{
		kind: "tool", toolName: "Task", toolID: "call-1", status: status,
		toolArgs: args, text: output, agent: children,
	}
}

func childSession(title, status string, in, out int, start time.Time, finish time.Time) protocol.ChildSession {
	return protocol.ChildSession{
		SessionID: "child-1", ParentCallID: "call-1", Title: title, Purpose: "task",
		Run: protocol.RunView{
			ID: "run-1", Status: status, WaitPolicy: "join", StartedAt: start, FinishedAt: finish,
			Usage: protocol.UsageSnapshot{InputTokens: in, OutputTokens: out},
		},
	}
}

// A running delegation shows a cyan marker with live stats and never dumps the
// child's internal output into the parent transcript.
func TestAgentMarkerRunning(t *testing.T) {
	start := time.Now().Add(-30 * time.Second)
	child := childSession("research auth handlers", "running", 22000, 8000, start, time.Time{})
	item := agentTaskItem("running", map[string]any{"description": "research auth handlers"}, "", child)

	got := sanitizeANSI(renderItemWidth(&item, 100))
	if !strings.Contains(got, "◆") || !strings.Contains(got, "Task") {
		t.Fatalf("running marker missing glyph/name:\n%s", got)
	}
	if !strings.Contains(got, "research auth handlers") {
		t.Fatalf("running marker missing description:\n%s", got)
	}
	if !strings.Contains(got, "running") {
		t.Fatalf("running marker missing state:\n%s", got)
	}
	if !strings.Contains(got, "30k tokens") {
		t.Fatalf("running marker missing token stats:\n%s", got)
	}
	if strings.Contains(got, "Done") {
		t.Fatalf("running marker must not read Done:\n%s", got)
	}
}

// A completed delegation shows Done(tokens · duration) plus a bounded preview of
// the final answer, not the whole answer.
func TestAgentMarkerDonePreviewsAnswer(t *testing.T) {
	start := time.Now().Add(-63 * time.Second)
	finish := start.Add(63 * time.Second)
	child := childSession("research auth handlers", "succeeded", 40000, 5200, start, finish)
	child.Run.Output = "short answer"
	answer := strings.Repeat("A", 600)
	item := agentTaskItem("success", map[string]any{"description": "research auth handlers"}, answer, child)

	got := sanitizeANSI(renderItemWidth(&item, 100))
	if !strings.Contains(got, "✓") || !strings.Contains(got, "Done") {
		t.Fatalf("done marker missing glyph/Done:\n%s", got)
	}
	if !strings.Contains(got, "45.2k tokens") {
		t.Fatalf("done marker missing token total:\n%s", got)
	}
	if !strings.Contains(got, "1m 03s") {
		t.Fatalf("done marker missing duration:\n%s", got)
	}
	if !strings.Contains(got, "/transcript") || !strings.Contains(got, "/agents") {
		t.Fatalf("done marker missing full-output/switch hints:\n%s", got)
	}
	// The 600-cell answer must be bounded to the 240-cell preview.
	if strings.Count(got, "A") > 245 {
		t.Fatalf("done marker dumped the full answer (%d As):\n%s", strings.Count(got, "A"), got)
	}
	if !strings.Contains(got, "…") {
		t.Fatalf("done marker preview was not truncated:\n%s", got)
	}
}

// A failed delegation surfaces the error summary in red, not the answer.
func TestAgentMarkerError(t *testing.T) {
	start := time.Now().Add(-5 * time.Second)
	child := childSession("audit deps", "failed", 1000, 200, start, time.Time{})
	child.Run.Error = "child crashed: model unavailable"
	item := agentTaskItem("error", map[string]any{"description": "audit deps"}, "tool failed", child)

	got := sanitizeANSI(renderItemWidth(&item, 100))
	if !strings.Contains(got, "✗") {
		t.Fatalf("error marker missing glyph:\n%s", got)
	}
	if !strings.Contains(got, "child crashed: model unavailable") {
		t.Fatalf("error marker missing child error summary:\n%s", got)
	}
}

// A read-only guardian child is a one-shot inspection and skips the usage tail.
func TestAgentMarkerGuardianSkipsUsage(t *testing.T) {
	start := time.Now().Add(-12 * time.Second)
	guardian := childSession("verify no secrets logged", "succeeded", 4000, 900, start, start.Add(12*time.Second))
	guardian.Purpose = "guardian"
	guardian.Run.Output = "clean"
	gItem := agentTaskItem("success", map[string]any{"description": "verify no secrets logged"}, "clean", guardian)
	got := sanitizeANSI(renderItemWidth(&gItem, 100))
	if strings.Contains(got, "tokens") || strings.Contains(got, "12s") {
		t.Fatalf("guardian marker should skip the usage tail:\n%s", got)
	}
	if !strings.Contains(got, "Done") {
		t.Fatalf("guardian marker missing Done state:\n%s", got)
	}

	task := childSession("research auth handlers", "succeeded", 4000, 900, start, start.Add(12*time.Second))
	task.Purpose = "task"
	tItem := agentTaskItem("success", map[string]any{"description": "research auth handlers"}, "answer", task)
	if got := sanitizeANSI(renderItemWidth(&tItem, 100)); !strings.Contains(got, "tokens") {
		t.Fatalf("task marker should keep the usage tail:\n%s", got)
	}
}

// A batch fan-out renders one header plus a line per child, ordered by batch.
func TestAgentMarkerBatchGroup(t *testing.T) {
	start := time.Now().Add(-40 * time.Second)
	first := childSession("research auth", "running", 22000, 8000, start, time.Time{})
	first.BatchIndex = 0
	second := childSession("draft migration", "succeeded", 18000, 4000, start, start.Add(40*time.Second))
	second.BatchIndex = 1
	third := childSession("audit deps", "running", 9000, 2000, start, time.Time{})
	third.BatchIndex = 2
	item := agentTaskItem("running", map[string]any{"agents": []any{
		map[string]any{"description": "research auth"},
		map[string]any{"description": "draft migration"},
		map[string]any{"description": "audit deps"},
	}}, "", first, second, third)

	got := sanitizeANSI(renderItemWidth(&item, 100))
	if !strings.Contains(got, "3 agents") {
		t.Fatalf("batch header missing agent count:\n%s", got)
	}
	for _, want := range []string{"research auth", "draft migration", "audit deps"} {
		if !strings.Contains(got, want) {
			t.Fatalf("batch group missing child %q:\n%s", want, got)
		}
	}
	// Order must follow BatchIndex.
	if strings.Index(got, "research auth") > strings.Index(got, "draft migration") ||
		strings.Index(got, "draft migration") > strings.Index(got, "audit deps") {
		t.Fatalf("batch children out of order:\n%s", got)
	}
	// Tree glyphs: ├─ for non-last children, └─ for the last.
	if strings.Count(got, "├─") != 2 || strings.Count(got, "└─") != 1 {
		t.Fatalf("batch group missing tree branch glyphs:\n%s", got)
	}
}

// Once every child of a batch succeeds, the header folds to "N agents finished"
// with an aggregate token total and wall duration.
func TestAgentMarkerBatchFinished(t *testing.T) {
	start := time.Now().Add(-90 * time.Second)
	first := childSession("research auth", "succeeded", 20000, 6000, start, start.Add(50*time.Second))
	first.BatchIndex = 0
	second := childSession("draft migration", "succeeded", 12000, 3000, start, start.Add(90*time.Second))
	second.BatchIndex = 1
	item := agentTaskItem("success", map[string]any{"agents": []any{
		map[string]any{"description": "research auth"},
		map[string]any{"description": "draft migration"},
	}}, "", first, second)

	got := sanitizeANSI(renderItemWidth(&item, 100))
	if !strings.Contains(got, "2 agents finished") {
		t.Fatalf("finished batch header missing 'finished':\n%s", got)
	}
	if !strings.Contains(got, "41k tokens") {
		t.Fatalf("finished batch header missing aggregate tokens:\n%s", got)
	}
	if !strings.Contains(got, "1m 30s") {
		t.Fatalf("finished batch header missing wall duration:\n%s", got)
	}
	if !strings.Contains(got, "✓") {
		t.Fatalf("finished batch missing success glyph:\n%s", got)
	}
	if strings.Count(got, "Done") < 2 {
		t.Fatalf("finished batch children should each read Done:\n%s", got)
	}
}

// The Agent control tool collapses its (often large) JSON result rather than
// dumping it inline.
func TestAgentControlToolCollapsesOutput(t *testing.T) {
	item := historyCell{
		kind: "tool", toolName: "Agent", toolID: "call-9", status: "success",
		toolArgs: map[string]any{"action": "list"},
		text:     strings.Repeat(`{"session_id":"x"}`, 80),
	}
	got := sanitizeANSI(renderItemWidth(&item, 100))
	if !strings.Contains(got, "Agent") || !strings.Contains(got, "list") {
		t.Fatalf("agent control marker missing name/action:\n%s", got)
	}
	// The source repeats the fragment 80 times; the bounded preview keeps only
	// roughly the first 240 cells worth and must truncate the rest.
	if n := strings.Count(got, "session_id"); n >= 80 || n > 20 {
		t.Fatalf("agent control tool dumped its full JSON result (%d fragments):\n%s", n, got)
	}
	if !strings.Contains(got, "…") {
		t.Fatalf("agent control tool preview was not truncated:\n%s", got)
	}
}

// childSessionForCallID resolves children by ParentCallID and orders a batch by
// BatchIndex even when the directory returns them out of order.
func TestChildSessionForCallIDOrdersBatch(t *testing.T) {
	m := &Model{routing: &sessionRouting{rows: []protocol.ChildSession{
		{SessionID: "c", ParentCallID: "call-1", BatchIndex: 2},
		{SessionID: "a", ParentCallID: "call-1", BatchIndex: 0},
		{SessionID: "other", ParentCallID: "call-2", BatchIndex: 0},
		{SessionID: "b", ParentCallID: "call-1", BatchIndex: 1},
	}}}
	got := m.childSessionForCallID("call-1")
	if len(got) != 3 {
		t.Fatalf("resolved %d children, want 3", len(got))
	}
	if got[0].SessionID != "a" || got[1].SessionID != "b" || got[2].SessionID != "c" {
		t.Fatalf("children not ordered by BatchIndex: %v %v %v", got[0].SessionID, got[1].SessionID, got[2].SessionID)
	}
	if m.childSessionForCallID("") != nil {
		t.Fatal("empty call id must resolve to nil")
	}
	if m.childSessionForCallID("missing") != nil {
		t.Fatal("unknown call id must resolve to nil")
	}
}

// A running delegation stays in the managed live tail and must never stream raw
// child text into native scrollback; on completion the compact marker is
// printed once, not the child's full answer.
func TestInlineFlushRendersAgentMarkerNotRawOutput(t *testing.T) {
	m := inlineTestModel()
	m.width, m.height = 100, 24
	m.routing = &sessionRouting{generation: 7}
	m.inline.forgetAll()
	m.inline.prime(nil)
	m.inline.showBaseline = false
	m.turnDone = false

	start := time.Now().Add(-63 * time.Second)
	child := childSession("research auth handlers", "running", 22000, 8000, start, time.Time{})
	answer := strings.Repeat("A", 600)
	args := map[string]any{"description": "research auth handlers"}

	running := agentTaskItem("running", args, "", child)
	running.messageID = "item-task"
	m.items = []historyCell{running}
	if cmd := planTestHistory(m); cmd != nil {
		t.Fatal("running delegation was printed to native scrollback")
	}
	if off := m.inline.offsets[inlineLogicalKey(running, 0)]; off != 0 {
		t.Fatalf("running delegation advanced its print offset to %d", off)
	}

	child.Run.Status = "succeeded"
	child.Run.FinishedAt = start.Add(63 * time.Second)
	child.Run.Output = answer
	done := agentTaskItem("success", args, answer, child)
	done.messageID = "item-task"
	m.items = []historyCell{done}
	m.turnDone = true

	cmd := planTestHistory(m)
	if cmd == nil {
		t.Fatal("completed delegation was not printed")
	}
	printed, ok := cmd().(inlinePrintMsg)
	if !ok {
		t.Fatalf("unexpected print message %T", cmd())
	}
	text := sanitizeANSI(printed.text)
	if !strings.Contains(text, "✓") || !strings.Contains(text, "Done") {
		t.Fatalf("scrollback did not receive the agent marker:\n%s", text)
	}
	if n := strings.Count(text, "A"); n > 245 {
		t.Fatalf("scrollback received the full child answer (%d As):\n%s", n, text)
	}
	if cmd2 := planTestHistory(m); cmd2 != nil {
		t.Fatal("completed delegation was printed twice")
	}
}

// A multiplexed child event folds into the routing directory (replacing, never
// duplicating, by SessionID) and drives the parent Task marker's live stats
// without waiting for the polling fallback.
func TestApplyChildUpdateDrivesParentMarker(t *testing.T) {
	m := inlineTestModel()
	m.width, m.height = 100, 24
	m.sessionID = "root"
	m.routing = &sessionRouting{rootID: "root"}

	start := time.Now().Add(-30 * time.Second)
	args := map[string]any{"description": "research auth handlers"}
	parent := agentTaskItem("running", args, "")
	parent.messageID = "item-task"
	m.items = []historyCell{parent}

	running := childSession("research auth handlers", "running", 22000, 8000, start, time.Time{})
	m.applyChildUpdate(running)

	if len(m.routing.rows) != 1 {
		t.Fatalf("first child event did not populate the directory: %d rows", len(m.routing.rows))
	}
	got := sanitizeANSI(renderItemWidth(&m.items[0], 100))
	if !strings.Contains(got, "30k tokens") {
		t.Fatalf("marker did not pick up live stats from the child event:\n%s", got)
	}

	done := childSession("research auth handlers", "succeeded", 40000, 5200, start, start.Add(30*time.Second))
	done.Run.Output = "short answer"
	m.applyChildUpdate(done)
	// For a join delegation the parent tool-result is the completion signal that
	// flips the glyph; the child event carries the fresh stats.
	m.items[0].status = "success"

	if len(m.routing.rows) != 1 {
		t.Fatalf("child event duplicated the directory row instead of replacing it: %d rows", len(m.routing.rows))
	}
	got = sanitizeANSI(renderItemWidth(&m.items[0], 100))
	if !strings.Contains(got, "45.2k tokens") {
		t.Fatalf("marker did not refresh tokens from the updated child event:\n%s", got)
	}
	if !strings.Contains(got, "✓") || !strings.Contains(got, "Done") {
		t.Fatalf("marker did not flip to Done on the completed child event:\n%s", got)
	}
}
