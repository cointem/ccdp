package tui

import (
	"strings"
	"testing"

	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
	tea "github.com/charmbracelet/bubbletea"
)

func TestFooterDoesNotPresentDefaultEffortAsPermissionMode(t *testing.T) {
	m := sugModel()
	m.width = 100
	m.hasSnapshot = true
	m.snapshot.Settings.Permission.Mode = "acceptEdits"
	m.snapshot.Settings.ReasoningEffort = ""
	text := sanitizeANSI(m.renderPersistentStatus())
	if strings.Contains(text, "default") || strings.Count(text, "edits") != 1 {
		t.Fatalf("ambiguous footer: %s", text)
	}
	m.snapshot.Settings.ReasoningEffort = "high"
	if !strings.Contains(sanitizeANSI(m.renderPersistentStatus()), "high") {
		t.Fatal("explicit reasoning effort lost")
	}
}

func TestFooterKeepsDefaultPermissionAfterSwitchingBack(t *testing.T) {
	m := sugModel()
	m.width = 100
	m.hasSnapshot = true
	m.snapshot.Settings.ReasoningEffort = "default"
	for _, mode := range []string{"default", "acceptEdits", "default"} {
		m.snapshot.Settings.Permission.Mode = mode
		text := sanitizeANSI(m.renderPersistentStatus())
		if strings.Count(text, permissions.Mode(mode).Label()) != 1 {
			t.Fatalf("permission mode %q should appear exactly once: %s", mode, text)
		}
		if mode == "acceptEdits" && strings.Contains(text, "default") {
			t.Fatalf("default effort should stay hidden: %s", text)
		}
	}
}

func TestSnapshotClearsApprovalNoticeBeforeReceipt(t *testing.T) {
	m := sugModel()
	m.handleProtocolEvent(protocol.EventView{Kind: protocol.EventApprovalRequest, Approval: &protocol.ApprovalView{ID: "a", Tool: "Bash"}})
	snapshot := m.snapshot
	snapshot.Approval = nil
	snapshot.Plan = nil
	snapshot.Phase = protocol.PhaseIdle
	m.applySnapshot(snapshot)
	if strings.Contains(m.renderStatus(), "approval") || len(m.visibleNotices()) != 0 {
		t.Fatal("resolved decision left stale notice")
	}
}

func TestInitialApprovalEnterAllowsOnce(t *testing.T) {
	m := sugModel()
	client := m.client.(*recordingClient)
	m.approval = &approvalPrompt{ID: "a", Tool: "Bash", Command: "cat input.txt"}
	_, cmd := m.handleApprovalKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("initial focused choice did not submit")
	}
	cmd()
	request := client.submits[len(client.submits)-1].Approval
	if request == nil || !request.Approve || request.Remember {
		t.Fatal("initial choice was not Allow once")
	}
}

func TestFileLinkIsClickableAndEscapedInRenderedTranscript(t *testing.T) {
	for _, path := range []string{"/tmp/项目/file.go", "src/file.go"} {
		m := sugModel()
		m.workspace = "/tmp/project"
		item := historyCell{kind: "assistant", text: "See [file.go](" + path + ")"}
		rendered := m.renderLogItem(&item, 40)
		target := terminalLinkTarget(path, m.workspace)
		if target == "" || !strings.Contains(rendered, "\x1b]8;;"+target+"\x1b\\") || !strings.Contains(rendered, "\x1b]8;;\x1b\\") {
			t.Fatalf("missing bounded hyperlink: %q", rendered)
		}
		if strings.Contains(sanitizeANSI(rendered), "file://") {
			t.Fatal("URL unnecessarily repeated in visible text")
		}
	}
	for _, target := range []string{"javascript:alert(1)", "https://site/\x1b]0;injected\a"} {
		if terminalLinkTarget(target) != "" {
			t.Fatal("unsafe terminal destination accepted")
		}
	}
}

func TestWritePreviewDoesNotLookLikeDeletion(t *testing.T) {
	out := sanitizeANSI(renderFileChangePreview(toolPresentation{Name: "Write", Status: "success", Args: map[string]any{"content": "first\nsecond\n"}}, 80))
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "-") {
			t.Fatalf("false deletion: %q", line)
		}
	}
	if !strings.Contains(out, "1  first") {
		t.Fatal("content line number lost")
	}
}
