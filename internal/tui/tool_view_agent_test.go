package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"ccdp/internal/protocol"
)

func agentTaskItem(status string, args map[string]any, output string, children ...protocol.ChildSession) historyCell {
	item := historyCell{kind: "tool", toolName: "SpawnAgent", toolID: "call-1", status: status, toolArgs: args, text: output}
	if len(children) == 1 {
		item.agent = &children[0]
	}
	return item
}

func childSession(title, status string, in, out int, start time.Time, finish time.Time) protocol.ChildSession {
	return protocol.ChildSession{
		SessionID: "child-1", ParentCallID: "call-1", Title: title, Purpose: "task",
		Run: protocol.RunView{
			ID: "run-1", Status: status, WaitPolicy: "notify", StartedAt: start, FinishedAt: finish,
			Usage: protocol.UsageSnapshot{InputTokens: in, OutputTokens: out},
		},
	}
}

func TestSpawnAgentMarkerTracksChildWithoutLegacyWaitPolicy(t *testing.T) {
	for _, tc := range []struct{ status, label string }{
		{"queued", "排队中"},
		{"starting", "准备中"},
		{"running", "running"},
		{"waiting_approval", "needs approval"},
		{"settling", "正在收尾"},
		{"succeeded", "运行结束"},
		{"failed", "error"},
		{"cancelled", "stopped"},
		{"partial", "部分完成"},
		{"interrupted", "已中断"},
		{"unknown", "状态未知"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			start := time.Now().Add(-27 * time.Second)
			child := childSession("rust-list-worker", tc.status, 30000, 8000, start, start.Add(27*time.Second))
			child.Run.WaitPolicy, child.Run.ToolUses = "notify", 7
			child.Run.Error = "child failed to run"
			item := historyCell{
				kind: "tool", toolName: "SpawnAgent", status: "success",
				toolArgs: map[string]any{"task": "create a list", "name": "rust-list-worker"},
				text:     "SPAWN_ACCEPTANCE_JSON", agent: &child,
			}
			got := sanitizeANSI(renderItemWidth(&item, 160))
			if !strings.Contains(got, " · "+tc.label) || strings.Contains(got, "Done") {
				t.Fatalf("incorrect child status:\n%s", got)
			}
			if !strings.Contains(got, "7 tool uses · 38k tokens · 27s") {
				t.Fatalf("lost child statistics:\n%s", got)
			}
			if strings.Contains(got, "SPAWN_ACCEPTANCE_JSON") {
				t.Fatalf("acceptance receipt rendered as child answer:\n%s", got)
			}
			if tc.status == "failed" && !strings.Contains(got, child.Run.Error) {
				t.Fatalf("lost child error:\n%s", got)
			}
		})
	}
}

func TestSpawnAgentMarkerBeforeChildUpdate(t *testing.T) {
	for _, tc := range []struct{ status, label string }{
		{"running", "running"},
		{"success", "已启动"},
		{"error", "error"},
		{"denied", "error"},
		{"cancelled", "stopped"},
	} {
		item := historyCell{kind: "tool", toolName: "SpawnAgent", status: tc.status, text: "receipt or error"}
		got := sanitizeANSI(renderItemWidth(&item, 100))
		if !strings.Contains(got, " · "+tc.label) || strings.Contains(got, "Done") {
			t.Fatalf("%s before child update:\n%s", tc.status, got)
		}
		if tc.status == "success" && strings.Contains(got, item.text) {
			t.Fatalf("start receipt rendered as answer:\n%s", got)
		}
	}
}

func TestWaitAgentRunningHiddenByDefault(t *testing.T) {
	item := historyCell{kind: "tool", toolName: "WaitAgent", status: "running", toolArgs: map[string]any{}}
	if got := renderItemWidth(&item, 100); got != "" {
		t.Fatalf("routine wait visible: %s", got)
	}
	p := toolPresentationFromItem(&item)
	p.Expanded = true
	got := sanitizeANSI(renderToolPresentation(p, 100))
	if !strings.Contains(got, " · running") || strings.Contains(got, "已启动") {
		t.Fatalf("wait call misclassified as spawn:\n%s", got)
	}
}

func TestWaitAgentMarkerExplainsReturnWithoutImplyingCompletion(t *testing.T) {
	for _, tc := range []struct{ reason, status, label string }{
		{"collaboration", "", "收到协作消息"},
		{"attention", "", "需要审批"},
		{"user_input", "", "收到用户输入"},
		{"no_pending_work", "", "无待处理子任务"},
	} {
		out, err := json.Marshal(map[string]string{"reason": tc.reason})
		if err != nil {
			t.Fatal(err)
		}
		item := historyCell{kind: "tool", toolName: "WaitAgent", status: "success", text: string(out)}
		p := toolPresentationFromItem(&item)
		p.Expanded = true
		got := sanitizeANSI(renderToolPresentation(p, 150))
		if !strings.Contains(got, tc.label) || strings.Contains(got, "Done") || strings.Contains(got, "session_id") {
			t.Fatalf("misleading %s wait marker:\n%s", tc.reason, got)
		}
	}
	if got := agentWaitSummary(`{"truncated`); got != "本次等待结束" {
		t.Fatalf("invalid/truncated result should not imply completion: %s", got)
	}
}

func TestSpawnCardOmitsReceiptAndResultBodies(t *testing.T) {
	start := time.Now().Add(-63 * time.Second)
	child := childSession("research auth", "succeeded", 40000, 5200, start, start.Add(63*time.Second))
	child.Run.Output = "FULL_CHILD_REPORT"
	item := agentTaskItem("success", nil, "ACCEPTANCE_RECEIPT", child)
	got := sanitizeANSI(renderItemWidth(&item, 120))
	for _, want := range []string{"✓", "运行结束", "45.2k tokens", "1m 03s"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s: %s", want, got)
		}
	}
	if strings.Contains(got, child.Run.Output) || strings.Contains(got, item.text) {
		t.Fatal("card duplicated report/receipt")
	}
}

func TestAgentControlToolCollapsesOutput(t *testing.T) {
	item := historyCell{kind: "tool", toolName: "ListAgents", toolID: "call-9", status: "success",
		text: strings.Repeat(`{"agent_id":"x"}`, 80)}
	got := sanitizeANSI(renderItemWidth(&item, 100))
	if strings.Count(got, "agent_id") > 20 || !strings.Contains(got, "…") {
		t.Fatalf("control result not bounded: %s", got)
	}
}

func TestChildSessionForCallIDIsOneToOne(t *testing.T) {
	m := &Model{routing: &sessionRouting{rows: []protocol.ChildSession{
		{SessionID: "a", ParentCallID: "call-1"},
		{SessionID: "b", ParentCallID: "call-2"},
	}}}
	got := m.childSessionForCallID("call-1")
	if got == nil || got.SessionID != "a" {
		t.Fatalf("wrong session: %+v", got)
	}
	if m.childSessionForCallID("") != nil || m.childSessionForCallID("missing") != nil {
		t.Fatal("unknown call resolved")
	}
}

func TestLegacyAgentToolNamesAreNotSpecialCased(t *testing.T) {
	for _, name := range []string{"Task", "Agent", "Subagent"} {
		if isAgentTool(name) {
			t.Fatalf("legacy tool still has a custom renderer: %s", name)
		}
	}
	// No legacy wait_policy can turn an inspection receipt into a live task.
	p := toolPresentation{Name: "ListAgents", Status: "success", Args: map[string]any{"wait_policy": "notify"}, Output: "{}"}
	if got := sanitizeANSI(renderAgentPresentation(p, 100)); strings.Contains(got, "已启动") || strings.Contains(got, "running") {
		t.Fatal(got)
	}
}

func TestInlineFlushRendersAgentMarkerNotRawOutput(t *testing.T) {
	m := inlineTestModel()
	m.width, m.height = 120, 24
	m.routing = &sessionRouting{generation: 7}
	m.inline.forgetAll()
	m.inline.prime(nil)
	m.inline.showBaseline = false
	m.turnDone = false
	start := time.Now().Add(-63 * time.Second)
	child := childSession("research auth", "running", 22000, 8000, start, time.Time{})
	// Spawn's receipt is complete while the child itself is still running.
	item := agentTaskItem("success", nil, "receipt", child)
	item.messageID = "spawn-item"
	m.items = []historyCell{item}
	if cmd := planTestHistory(m); cmd != nil {
		t.Fatal("running card printed as final")
	}
	child.Run.Status, child.Run.FinishedAt = "succeeded", start.Add(63*time.Second)
	done := agentTaskItem("success", nil, "RAW_RECEIPT", child)
	done.messageID = "spawn-item"
	m.items, m.turnDone = []historyCell{done}, true
	cmd := planTestHistory(m)
	if cmd == nil {
		t.Fatal("completed card not printed")
	}
	printed, ok := cmd().(inlinePrintMsg)
	if !ok {
		t.Fatal("missing print message")
	}
	text := sanitizeANSI(printed.text)
	if !strings.Contains(text, "运行结束") || strings.Contains(text, "RAW_RECEIPT") {
		t.Fatal(text)
	}
	if planTestHistory(m) != nil {
		t.Fatal("card printed twice")
	}
}

func TestApplyChildUpdateDrivesParentMarker(t *testing.T) {
	m := inlineTestModel()
	m.width, m.height, m.sessionID = 120, 24, "root"
	m.routing = &sessionRouting{rootID: "root"}
	item := agentTaskItem("success", map[string]any{"task": "research auth"}, "receipt")
	item.messageID = "spawn-item"
	m.items = []historyCell{item}
	start := time.Now().Add(-30 * time.Second)
	m.applyChildUpdate(childSession("research auth", "running", 22000, 8000, start, time.Time{}))
	if got := sanitizeANSI(renderItemWidth(&m.items[0], 120)); !strings.Contains(got, "30k tokens") || !strings.Contains(got, "running") {
		t.Fatal(got)
	}
	m.applyChildUpdate(childSession("research auth", "succeeded", 40000, 5200, start, start.Add(30*time.Second)))
	if len(m.routing.rows) != 1 {
		t.Fatal("duplicate child card")
	}
	if got := sanitizeANSI(renderItemWidth(&m.items[0], 120)); !strings.Contains(got, "45.2k tokens") || !strings.Contains(got, "运行结束") {
		t.Fatal(got)
	}
}
