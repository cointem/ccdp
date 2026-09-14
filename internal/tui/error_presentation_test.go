package tui

import (
	"testing"

	"ccdp/internal/protocol"
)

func TestTurnErrorHasOneTranscriptRowAndNoRepeatedNotice(t *testing.T) {
	for _, snapshotFirst := range []bool{false, true} {
		name := "event first"
		if snapshotFirst {
			name = "snapshot first"
		}
		t.Run(name, func(t *testing.T) {
			m := astraModel(t, 80, 24)
			m.routing = &sessionRouting{rootID: m.sessionID}
			m.turnStarted = now()
			text := "LLM error: llm: 401 Unauthorized: missing API key"
			row := protocol.TranscriptItem{ID: "error:turn-1", Kind: "error", TurnID: "turn-1", Text: text, Status: "failed"}
			event := protocol.EventView{Kind: protocol.EventError, SessionID: protocol.SessionID(m.sessionID), TurnID: "1", Error: text}
			if snapshotFirst {
				m.applyTranscript([]protocol.TranscriptItem{row})
			}
			m.handleProtocolEvent(event)
			if !snapshotFirst {
				if len(m.reports) != 1 {
					t.Fatalf("live reports = %d", len(m.reports))
				}
				old := m.reports[0]
				m.inline.mark(old, 0)
				m.applyTranscript([]protocol.TranscriptItem{row})
				if _, ok := m.inline.printed[itemIdentity(m.confirmedItems[0], 0)]; !ok {
					t.Fatal("durable error lost native print acknowledgement")
				}
			}
			m.handleProtocolEvent(event) // replay must not create a second report
			m.handleProtocolEvent(protocol.EventView{Kind: protocol.EventTurnDone, TurnID: "1"})
			count := 0
			for _, item := range m.items {
				if item.kind == "error" && item.text == text {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("error rows=%d, want 1", count)
			}
			if status := sanitizeANSI(m.renderStatus()); status != "" {
				t.Fatalf("failed turn retained redundant status lane: %q", status)
			}
			if len(m.visibleNotices()) != 0 {
				t.Fatalf("unexpected redundant notices: %#v", m.visibleNotices())
			}
			// Equal provider failures in later turns and different diagnostics in the
			// same turn remain separate; this is identity reconciliation, not text-only dedup.
			event.TurnID = "2"
			m.handleProtocolEvent(event)
			event.Error = "another failure"
			m.handleProtocolEvent(event)
			if len(m.items) != 3 {
				t.Fatalf("distinct failures lost: %#v", m.items)
			}
		})
	}
}

func TestFailedInputDoesNotRepeatConfirmedTurnError(t *testing.T) {
	m := astraModel(t, 80, 24)
	text := "LLM error: unauthorized"
	cmd := protocol.NewSubmitInput("input-op", protocol.SessionID(m.sessionID), "input", "hello", protocol.InputSteer)
	m.registerOperation(cmd, "message")
	op := m.operations[cmd.ID]
	op.TurnID = "1"
	m.operations[cmd.ID] = op
	m.handleProtocolEvent(protocol.EventView{Kind: protocol.EventError, TurnID: "1", Error: text})
	m.finishInputOperations("1", protocol.TurnFailed, "")
	if len(m.visibleNotices()) != 0 {
		t.Fatal("terminal receipt repeated transcript error")
	}
}
