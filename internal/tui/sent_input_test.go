package tui

import (
	"testing"

	"ccdp/internal/protocol"
)

func TestConsumedInputStaysClearedAfterTurnFailure(t *testing.T) {
	for _, terminal := range []string{"error event", "failed snapshot", "cancelled snapshot"} {
		for _, nextDraft := range []string{"", "next message"} {
			t.Run(terminal+"/draft="+nextDraft, func(t *testing.T) {
				m := sugModel()
				client := m.client.(*recordingClient)
				m.textarea.SetValue("sent message")
				m.inputImages = []protocol.InputImage{clipboardPNG(t)}
				_, submit := m.submit()
				_ = submit()
				sent := client.submits[0]
				if m.textarea.Value() != "" || len(m.inputImages) != 0 {
					t.Fatal("submit did not clear composer")
				}
				m.applyReceipt(protocol.Receipt{CommandID: sent.ID, SessionID: sent.SessionID, Status: protocol.ReceiptScheduled}, "")
				m.handleProtocolEvent(protocol.EventView{Kind: protocol.EventUserMessage, SessionID: sent.SessionID, TurnID: "1", Text: "sent message"})
				m.textarea.SetValue(nextDraft)
				if nextDraft != "" {
					m.inputImages = []protocol.InputImage{clipboardPNG(t)}
				}
				if terminal == "error event" {
					m.handleProtocolEvent(protocol.EventView{Kind: protocol.EventError, SessionID: sent.SessionID, TurnID: "1", Error: "LLM error: 401 Unauthorized"})
					m.handleProtocolEvent(protocol.EventView{Kind: protocol.EventTurnDone, SessionID: sent.SessionID, TurnID: "1"})
				} else {
					snapshot := m.snapshot
					snapshot.SessionID = sent.SessionID
					snapshot.Phase, snapshot.Busy = protocol.PhaseIdle, false
					outcome := protocol.TurnFailed
					if terminal == "cancelled snapshot" {
						outcome = protocol.TurnCancelled
					}
					snapshot.LastTurn = &protocol.TurnOutcome{TurnID: "turn-1", Status: outcome, Error: "401 Unauthorized"}
					m.applySnapshot(snapshot)
				}
				// Delayed command acknowledgements cannot restore or resurrect this input.
				m.applyReceipt(protocol.Receipt{CommandID: sent.ID, SessionID: sent.SessionID, Status: protocol.ReceiptScheduled}, "")
				if m.textarea.Value() != nextDraft {
					t.Fatalf("sent message restored over composer: %q", m.textarea.Value())
				}
				wantImages := 0
				if nextDraft != "" {
					wantImages = 1
				}
				if len(m.inputImages) != wantImages {
					t.Fatalf("composer attachments changed: %d", len(m.inputImages))
				}
				if len(m.pendingSubmissions) != 0 || len(m.pendingImages) != 0 || m.retryCommandID != "" {
					t.Fatal("consumed input retained as pending/retry")
				}
				if op := m.operations[sent.ID]; op.Active() {
					t.Fatal("turn did not close input operation")
				}
				// Re-entering the same text intentionally starts a fresh message.
				m.textarea.SetValue("sent message")
				_, again := m.submit()
				_ = again()
				if client.submits[len(client.submits)-1].ID == sent.ID {
					t.Fatal("new message reused consumed command ID")
				}
			})
		}
	}
}
