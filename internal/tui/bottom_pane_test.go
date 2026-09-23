package tui

import (
	"fmt"
	"strings"
	"testing"

	"ccdp/internal/protocol"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestDecisionInitialFocusAndDeferredDraft(t *testing.T) {
	m := sugModel()
	m.textarea.SetValue("unfinished draft")
	m.approval = &approvalPrompt{ID: "first", Tool: "Bash", Command: "echo first"}
	if m.selectedApprovalChoice() != 0 || !strings.Contains(m.renderApprovalInline(), "❯ Allow once") {
		t.Fatal("initial approval focus missing")
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.approvalActive() || !m.composerVisible() || m.textarea.Value() != "unfinished draft" {
		t.Fatal("dismissal must preserve decision and draft")
	}
	m.openPendingDecision()
	if !m.approvalActive() || m.selectedApprovalChoice() != 0 {
		t.Fatal("reopened approval must focus Allow once")
	}
	_, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil || !m.approvalPending {
		t.Fatal("explicit selection must submit and stay pending")
	}
	receipt := cmd().(commandReceiptMsg)
	m.approval = &approvalPrompt{ID: "second", Tool: "Bash", Command: "echo second"}
	m.applyReceipt(receipt.receipt, receipt.purpose)
	if m.approval == nil || m.approval.ID != "second" || m.selectedApprovalChoice() != 0 {
		t.Fatal("late receipt must preserve next approval and its initial focus")
	}
}

func TestApprovalReceiptClearsDecisionNoticeWithoutClobberingRuntimeActivity(t *testing.T) {
	m := sugModel()
	m.approval = &approvalPrompt{ID: "first", Tool: "Bash"}
	m.pendingApprovalKey = m.approvalKey()
	m.approvalPending = true
	m.pendingApprovalCommand = "approve-1"
	m.addNotice(Notice{Severity: NoticeDecision, Text: "awaiting approval…", Sticky: true})
	m.setActivity(Activity{SessionID: protocol.SessionID(m.sessionID), Phase: ActivityRunningTool, Label: "正在运行工具"})
	m.applyReceipt(protocol.Receipt{CommandID: "approve-1", SessionID: protocol.SessionID(m.sessionID), Status: protocol.ReceiptApplied}, "approval submitted")
	if m.approval != nil || m.approvalPending {
		t.Fatal("approval should close after its receipt")
	}
	for _, notice := range m.notices {
		if notice.Severity == NoticeDecision {
			t.Fatal("resolved approval notice remained visible")
		}
	}
	if m.activity.Phase != ActivityRunningTool || m.activity.Label != "正在运行工具" {
		t.Fatalf("late receipt replaced runtime activity: %#v", m.activity)
	}
}

func TestDisconnectedDecisionDoesNotBecomePending(t *testing.T) {
	m := sugModel()
	m.watchDisconnected = true
	m.approval = &approvalPrompt{ID: "first", Tool: "Bash"}
	m.selectApprovalChoice(1)
	if _, cmd := m.handleApprovalKey(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil || m.approvalPending {
		t.Fatal("offline approval must remain retryable")
	}
	m.textarea.SetValue("offline draft")
	if _, cmd := m.submit(); cmd != nil || m.textarea.Value() != "offline draft" {
		t.Fatal("offline input must remain in composer")
	}
	if cmd := m.requestInterrupt(); cmd != nil || m.interruptRequested {
		t.Fatal("offline interruption must not become permanently pending")
	}
}

func TestReconnectIsBoundedAndScoped(t *testing.T) {
	m := sugModel()
	for i := 1; i <= 5; i++ {
		if m.scheduleReconnect() == nil || m.watchRetry != i {
			t.Fatalf("retry %d not scheduled", i)
		}
	}
	if m.scheduleReconnect() != nil {
		t.Fatal("automatic retries must be bounded")
	}
	if _, cmd := m.update(reconnectMsg{sessionID: "other", generation: m.watchGeneration}); cmd != nil {
		t.Fatal("stale session retry must not attach")
	}
	generation := m.watchGeneration
	cmd := m.reconnect()
	if cmd == nil || m.watchGeneration == generation || m.watchRetry != 0 {
		t.Fatal("manual reconnect must invalidate previous attempts")
	}
	m.handleWatchOpened(cmd().(watchOpenedMsg))
	if m.client.(*recordingClient).submitCount() != 0 {
		t.Fatal("reconnect must never replay commands")
	}
	if m.subscription != nil {
		_ = m.subscription.Close()
	}
}

func TestBottomPaneApprovalDimensions(t *testing.T) {
	for _, size := range [][2]int{{120, 40}, {80, 24}, {40, 16}, {24, 12}} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			m := astraModel(t, size[0], size[1])
			m.approval = &approvalPrompt{ID: "long", Tool: "Bash", Command: strings.Repeat("中英文 very long command\n", 40) + "END_SENTINEL"}
			m.layout()
			m.handleApprovalKey(tea.KeyMsg{Type: tea.KeyEnd})
			view := m.View()
			if m.renderInput() != "" {
				t.Fatal("approval must replace composer")
			}
			if !strings.Contains(view, "END_SENTINEL") || !strings.Contains(view, "confirm") {
				t.Fatalf("review end and controls must stay visible: %q", view)
			}
			if lipgloss.Height(view) > size[1] || lipgloss.Width(view) > size[0] {
				t.Fatalf("frame overflow: %dx%d", lipgloss.Width(view), lipgloss.Height(view))
			}
		})
	}
}

func TestQuitRunningRequiresExplicitConfirmation(t *testing.T) {
	m := sugModel()
	m.busy = true
	m.textarea.SetValue("draft")
	if m.requestQuit() != nil || !m.exitConfirm {
		t.Fatal("running work requires confirmation")
	}
	if _, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Fatal("Enter must not confirm exit")
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.exitConfirm || m.textarea.Value() != "draft" {
		t.Fatal("cancel exit must preserve draft")
	}
}

func TestFollowupUsesProtocolStrategy(t *testing.T) {
	m := sugModel()
	m.textarea.SetValue("next turn")
	_, cmd := m.submitInput(protocol.InputFollowup)
	if cmd == nil {
		t.Fatal("expected submission")
	}
	cmd()
	client := m.client.(*recordingClient)
	if len(client.submits) != 1 || client.submits[0].Input.Strategy != protocol.InputFollowup {
		t.Fatal("followup must use protocol strategy")
	}
}
