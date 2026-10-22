package tui

import (
	"fmt"
	"strings"
	"testing"

	"ccdp/internal/protocol"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestFollowupStaysByComposerUntilConsumed(t *testing.T) {
	m := sugModel()
	m.width, m.height, m.busy = 80, 24, true
	m.textarea.SetValue("queued-next-request")
	_, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyTab})
	if cmd == nil || m.textarea.Value() != "" {
		t.Fatal("Tab did not submit and clear the composer")
	}
	id := m.operations[m.latestOperation].InputID
	checkPending := func() {
		t.Helper()
		m.layout()
		if got := m.visiblePendingInputs(); len(got) != 1 || got[0].ID != id {
			t.Fatalf("pending preview missing/duplicated: %+v", got)
		}
		if !strings.Contains(m.composeChrome().input, "queued-next-request") || strings.Count(m.View(), "queued-next-request") != 1 {
			t.Fatal("queued text must stay in composer chrome exactly once")
		}
		for _, cell := range m.items {
			if strings.Contains(cell.text, "queued-next-request") {
				t.Fatal("queued input entered scrollable transcript")
			}
		}
	}
	checkPending() // Includes the interval before the first receipt/snapshot.
	snapshot := m.snapshot
	snapshot.Busy, snapshot.Phase = true, protocol.PhaseStreaming
	snapshot.PendingInputs = []protocol.InputView{{ID: id, Text: "queued-next-request", Strategy: protocol.InputFollowup}}
	m.applySnapshot(snapshot)
	m.applyReceipt(protocol.Receipt{CommandID: m.latestOperation, SessionID: snapshot.SessionID, Status: protocol.ReceiptScheduled}, "message submitted")
	m.textarea.SetValue("new draft")
	for i := 0; i < 30; i++ {
		m.handleProtocolEvent(protocol.EventView{Kind: protocol.EventStream, Text: "more output", Transcript: &protocol.TranscriptItem{
			ID: fmt.Sprintf("output-%d", i), Kind: "assistant", TurnID: "turn-1", Text: "more output", Status: "completed",
		}})
	}
	m.handleProtocolEvent(protocol.EventView{Kind: protocol.EventTurnDone, TurnID: "turn-1"})
	checkPending() // Clearing notices and finishing the old turn cannot hide it.
	m.handleProtocolEvent(protocol.EventView{Kind: protocol.EventUserMessage, MessageID: protocol.InputMessageID(id), TurnID: "turn-2", Text: "queued-next-request"})
	if got := m.visiblePendingInputs(); len(got) != 0 {
		t.Fatalf("consumed input remained pinned ahead of snapshot: %+v", got)
	}
	last := m.items[len(m.items)-1]
	if last.messageID != protocol.InputMessageID(id) || last.turnID != "turn-2" || m.textarea.Value() != "new draft" {
		t.Fatalf("input misplaced or new draft overwritten: %+v", last)
	}
	// A late scheduled receipt cannot resurrect the consumed preview.
	m.applyReceipt(protocol.Receipt{CommandID: m.latestOperation, SessionID: snapshot.SessionID, Status: protocol.ReceiptScheduled, Revision: protocol.Revision{LogSeq: 9}}, "message submitted")
	if len(m.visiblePendingInputs()) != 0 {
		t.Fatal("late receipt resurrected consumed input")
	}
}

func TestPendingInputsRestoredFromSnapshotAndFitFrame(t *testing.T) {
	for _, size := range [][2]int{{24, 12}, {40, 16}, {80, 24}, {120, 40}} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			m := astraModel(t, size[0], size[1])
			snapshot := m.snapshot
			for i := 0; i < 5; i++ {
				snapshot.PendingInputs = append(snapshot.PendingInputs, protocol.InputView{ID: protocol.InputID(fmt.Sprint(i)), Strategy: protocol.InputFollowup, Text: strings.Repeat("中文长消息\n", 30)})
			}
			m.applySnapshot(snapshot)
			m.layout()
			view := m.View()
			if len(m.visiblePendingInputs()) != 5 || !strings.Contains(view, "follow_up") || !strings.Contains(view, "draft") {
				t.Fatalf("restored queue or composer missing: %q", view)
			}
			if lipgloss.Width(view) > size[0] || lipgloss.Height(view) > size[1] {
				t.Fatalf("pending queue overflowed %v", size)
			}
			snapshot.PendingInputs = nil
			m.applySnapshot(snapshot)
			if m.renderPendingInputs() != "" {
				t.Fatal("cleared runtime queue remained visible")
			}
		})
	}
}

func TestRejectedFollowupRestoresDraftWithoutPendingPreview(t *testing.T) {
	m := sugModel()
	m.busy = true
	m.textarea.SetValue("rejected draft")
	m.handleKey(tea.KeyMsg{Type: tea.KeyTab})
	m.applyReceipt(protocol.Receipt{CommandID: m.latestOperation, SessionID: protocol.SessionID(m.sessionID), Status: protocol.ReceiptRejected}, "")
	if len(m.visiblePendingInputs()) != 0 || m.textarea.Value() != "rejected draft" {
		t.Fatal("rejected input should only return to the composer")
	}
}
