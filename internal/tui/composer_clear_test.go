package tui

import (
	"strings"
	"testing"
	"time"

	"ccdp/internal/protocol"
	tea "github.com/charmbracelet/bubbletea"
)

func TestDoubleEscapeClearsDraftWithoutInterrupting(t *testing.T) {
	for _, busy := range []bool{false, true} {
		m := astraModel(t, 80, 24)
		m.busy = busy
		text := strings.Repeat("draft\n", 10)
		m.textarea.SetValue(text)
		m.recordPastedText(text)
		m.inputImages = []protocol.InputImage{clipboardPNG(t)}
		m.history, m.historyIdx = []string{"previous input"}, 0
		m.historyDraft = &composerDraft{text: "saved draft"}
		m.missingHistoryImages = true
		m.inputRetry = &inputRetry{}
		m.pasteRequest = "late-paste"
		m.layout()
		esc := tea.KeyMsg{Type: tea.KeyEsc}
		if _, cmd := m.handleKey(esc); cmd != nil || m.textarea.Value() != text || len(m.inputImages) != 1 {
			t.Fatal("first Escape must preserve the draft without interrupting")
		}
		if _, cmd := m.handleKey(esc); cmd != nil || m.hasInputDraft() || m.interruptRequested {
			t.Fatal("second Escape must clear the draft without interrupting")
		}
		if m.pasteFold != nil || m.inputRetry != nil || m.historyDraft != nil || m.historyIdx != len(m.history) || m.textarea.Height() != 1 {
			t.Fatal("clearing left stale composer state")
		}
		m.applyClipboardRead(clipboardReadMsg{request: "late-paste", content: ClipboardContent{Text: "stale"}})
		if m.hasInputDraft() || len(m.history) != 1 {
			t.Fatal("late paste resurrected the draft or input history was erased")
		}
	}
}

func TestDoubleEscapeRequiresConsecutivePresses(t *testing.T) {
	for _, interrupt := range []string{"key", "timeout", "modal"} {
		t.Run(interrupt, func(t *testing.T) {
			m := astraModel(t, 80, 24)
			m.textarea.SetValue("keep me")
			esc := tea.KeyMsg{Type: tea.KeyEsc}
			m.handleKey(esc)
			switch interrupt {
			case "key":
				m.handleKey(tea.KeyMsg{Type: tea.KeyLeft})
			case "timeout":
				m.lastEscape = time.Now().Add(-doubleEscapeInterval - time.Second)
			case "modal":
				m.approval = &approvalPrompt{ID: "approval", Tool: "Bash"}
				m.handleKey(esc)
			}
			m.handleKey(esc)
			if m.textarea.Value() != "keep me" {
				t.Fatal("unrelated or expired Escape cleared the draft")
			}
			m.handleKey(esc)
			if m.hasInputDraft() {
				t.Fatal("a fresh double Escape did not clear the draft")
			}
		})
	}
}

func TestDoubleEscapeHandlesCompletionAndImageOnlyDrafts(t *testing.T) {
	for _, text := range []string{"/mo", ""} {
		m := astraModel(t, 80, 24)
		m.textarea.SetValue(text)
		m.inputImages = []protocol.InputImage{clipboardPNG(t)}
		m.refreshCmdSuggest()
		m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
		if len(m.cmdSug) != 0 || !m.hasInputDraft() {
			t.Fatal("first Escape must dismiss completion and preserve the draft")
		}
		m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
		if m.hasInputDraft() {
			t.Fatal("second Escape did not clear the draft")
		}
		m.busy = true
		m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
		if !m.interruptRequested {
			t.Fatal("Escape with an empty composer no longer interrupts")
		}
	}
}

func TestPackedDoubleEscapeRespectsModalFocus(t *testing.T) {
	m := astraModel(t, 80, 24)
	m.textarea.SetValue("keep while approving")
	m.approval = &approvalPrompt{ID: "approval", Tool: "Bash"}
	esc := tea.KeyMsg{Type: tea.KeyEsc, Alt: true}
	m.handleKey(esc)
	if m.approvalActive() || !m.hasInputDraft() {
		t.Fatal("packed Escape must dismiss the modal and preserve the draft")
	}
	m.handleKey(esc)
	if m.hasInputDraft() {
		t.Fatal("packed double Escape did not clear the draft")
	}
}
