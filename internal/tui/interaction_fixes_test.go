package tui

import (
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
)

func TestCyclePermissionModeImmediateResponse(t *testing.T) {
	m := astraModel(t, 80, 24)
	m.client = &recordingClient{snapshot: m.snapshot}
	m.layout()

	// Initial mode is default
	if m.mode != permissions.ModeDefault && m.mode != "" {
		t.Fatalf("expected default mode initially, got %v", m.mode)
	}

	// Press Shift+Tab (mode cycle)
	cmd := m.cyclePermissionMode()
	if cmd == nil {
		t.Fatal("cyclePermissionMode should return submitCommand cmd")
	}
	if m.mode != permissions.ModeAcceptEdits {
		t.Fatalf("expected acceptEdits after first cycle, got %v", m.mode)
	}

	// Cycle again -> bypass
	m.cyclePermissionMode()
	if m.mode != permissions.ModeBypass {
		t.Fatalf("expected bypass after second cycle, got %v", m.mode)
	}

	// Cycle again -> default
	m.cyclePermissionMode()
	if m.mode != permissions.ModeDefault {
		t.Fatalf("expected default after third cycle, got %v", m.mode)
	}
}

func TestContextDetailPopoverToggle(t *testing.T) {
	m := astraModel(t, 80, 24)
	m.layout()

	if m.showContextDetail {
		t.Fatal("context detail popover should initially be closed")
	}

	// Toggle via Ctrl+K
	m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlK})
	if !m.showContextDetail {
		t.Fatal("Ctrl+K should open context detail popover")
	}

	view := sanitizeANSI(m.View())
	if !strings.Contains(view, "Context Window") {
		t.Fatalf("popover view should contain 'Context Window', got:\n%s", view)
	}
	if !strings.Contains(view, "System Prompt") || !strings.Contains(view, "System Tools") {
		t.Fatalf("popover view should contain 4-tier breakdown, got:\n%s", view)
	}

	// Close via Esc
	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.showContextDetail {
		t.Fatal("Esc should close context detail popover")
	}
}

func TestContextDetailZeroTokensNoMessages(t *testing.T) {
	m := astraModel(t, 80, 24)
	m.snapshot.ContextUsedTokens = 0
	m.snapshot.History = nil
	m.showContextDetail = true
	m.layout()

	view := sanitizeANSI(m.View())
	if !strings.Contains(view, "0 / 200,000 tokens") {
		t.Fatalf("popover should report 0 tokens, got:\n%s", view)
	}
	if strings.Contains(view, "<1%") || strings.Contains(view, " 1%") {
		t.Fatalf("popover should not show nonzero percentages when 0 tokens used, got:\n%s", view)
	}
	if !strings.Contains(view, "System Prompt") || !strings.Contains(view, "Messages") {
		t.Fatalf("popover should show tiers, got:\n%s", view)
	}
	if strings.Count(view, "0%") < 5 {
		t.Fatalf("popover should show 0%% for header and all tiers when 0 tokens, got:\n%s", view)
	}
}

func TestMouseClickInteractions(t *testing.T) {
	m := astraModel(t, 80, 24)
	m.client = &recordingClient{snapshot: m.snapshot}
	m.layout()

	// Click bottom-left (the visible model label)
	_, _ = m.handleMouse(tea.MouseMsg{
		X:      10,
		Y:      23, // footer line
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
	})
	if m.picker == nil || m.picker.action.Kind != selectorModel {
		t.Fatal("model label must open model selector")
	}
	if m.mode != permissions.ModeDefault && m.mode != "" {
		t.Fatal("model click changed permissions")
	}
	m.picker = nil

	// Click the actual context metric rather than the footer hints.
	x, y := footerTextPosition(t, &m, "200k")
	m.handleMouse(tea.MouseMsg{
		X:      x,
		Y:      y,
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
	})
	if !m.showContextDetail {
		t.Fatal("clicking context area in footer should open context popover")
	}

	// Click anywhere while popover is open closes it
	m.handleMouse(tea.MouseMsg{
		X:      40,
		Y:      10,
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
	})
	if m.showContextDetail {
		t.Fatal("clicking while popover is open should close it")
	}
}

func TestPendingChildApprovalInline(t *testing.T) {
	m := astraModel(t, 80, 24)
	m.routing = &sessionRouting{
		rootID: "root-session",
		rows: []protocol.ChildSession{
			{
				SessionID: "child-worker-1",
				Title:     "Data Processor",
				Run:       protocol.RunView{Status: "waiting_approval"},
				Approval: &protocol.ApprovalView{
					ID:     "appr-123",
					Tool:   "Bash",
					Args:   json.RawMessage(`{"command":"rm -rf /tmp/cache"}`),
					Reason: "needs permission to clear cache",
				},
			},
		},
	}
	m.sessionID = "root-session"
	m.layout()

	child := m.pendingChildApproval()
	if child == nil {
		t.Fatal("expected pendingChildApproval to find child-worker-1")
	}

	view := sanitizeANSI(m.View())
	if !strings.Contains(view, "1 pending · /pending") {
		t.Fatalf("expected pending reminder in view, got:\n%s", view)
	}
	if strings.Contains(view, "rm -rf /tmp/cache") {
		t.Fatalf("background command stole the foreground surface:\n%s", view)
	}
	if m.manager.Target(&m) != FocusComposer || !strings.Contains(view, "draft") {
		t.Fatalf("background approval stole composer focus:\n%s", view)
	}
}

func TestCompletedChildCannotLeavePhantomPendingApproval(t *testing.T) {
	m := astraModel(t, 80, 24)
	m.routing = &sessionRouting{rootID: "root-session", rows: []protocol.ChildSession{{
		SessionID: "child-worker-1", Run: protocol.RunView{Status: "waiting_approval"},
		Approval: &protocol.ApprovalView{ID: "approval-1", Tool: "Bash"},
	}}}
	m.sessionID = "root-session"
	if m.pendingChildApproval() == nil {
		t.Fatal("expected active child approval")
	}
	m.applyChildUpdate(protocol.ChildSession{SessionID: "child-worker-1", Run: protocol.RunView{Status: "succeeded"},
		Approval: &protocol.ApprovalView{ID: "approval-1", Tool: "Bash"}})
	if m.pendingChildApproval() != nil || strings.Contains(m.pendingDecisionSummary(), "pending") {
		t.Fatal("terminal child retained a phantom pending approval")
	}
}

func TestToolCollapseExpand(t *testing.T) {
	m := astraModel(t, 80, 24)
	m.items = []historyCell{{kind: "tool", toolID: "call-1", toolName: "Bash", text: "output"}}
	before := m.renderLogItem(&m.items[0], 80)
	m.toggleToolExpanded("call-1")
	if m.detail == nil {
		t.Fatal("missing detail")
	}
	if after := m.renderLogItem(&m.items[0], 80); after != before {
		t.Fatal("detail changed transcript")
	}
	m.toggleToolExpanded("call-1")
	if m.detail != nil {
		t.Fatal("second click did not close")
	}
}

func TestModeCyclePositionStability(t *testing.T) {
	m := astraModel(t, 100, 24)
	m.layout()

	// Mode 1: default
	line1 := sanitizeANSI(m.renderPersistentStatus())
	idxModel1 := strings.Index(line1, "deepseek-chat")

	// Mode 2: acceptEdits
	m.cyclePermissionMode()
	line2 := sanitizeANSI(m.renderPersistentStatus())
	idxModel2 := strings.Index(line2, "deepseek-chat")

	// Mode 3: bypass
	m.cyclePermissionMode()
	line3 := sanitizeANSI(m.renderPersistentStatus())
	idxModel3 := strings.Index(line3, "deepseek-chat")

	if idxModel1 != idxModel2 || idxModel2 != idxModel3 {
		t.Fatalf("model name shifted position across mode changes: %d, %d, %d\nline1: %s\nline2: %s\nline3: %s",
			idxModel1, idxModel2, idxModel3, line1, line2, line3)
	}
}
