package tui

import (
	"ccdp/internal/protocol"
	"testing"
)

func TestInputOperationMatchesIdentityNotRepeatedText(t *testing.T) {
	for _, text := range []string{"你好", "继续", ""} {
		t.Run(text, func(t *testing.T) {
			m := astraModel(t, 80, 24)
			m.registerOperation(protocol.NewSubmitInput("new-command", "acceptance-root", "new-input", text, protocol.InputSteer), "message")
			old := protocol.TranscriptItem{ID: protocol.InputMessageID("old-input"), Kind: "user", TurnID: "1", Text: text}
			m.associateInputOperationsFromSnapshot(protocol.SessionView{Transcript: []protocol.TranscriptItem{old}})
			m.finishInputOperations("1", protocol.TurnSucceeded, "")
			if op := m.operations["new-command"]; !op.Active() || op.TurnID != "" {
				t.Fatalf("old input claimed new operation: %+v", op)
			}
			current := protocol.TranscriptItem{ID: protocol.InputMessageID("new-input"), Kind: "user", TurnID: "2", Text: "transformed or truncated text"}
			m.associateInputOperationsFromSnapshot(protocol.SessionView{Transcript: []protocol.TranscriptItem{old, current}})
			m.finishInputOperations("2", protocol.TurnSucceeded, "")
			if op := m.operations["new-command"]; op.Active() {
				t.Fatalf("operation still pending: %+v", op)
			}
		})
	}
}

func TestInputEventMatchesIdentityWithChangedText(t *testing.T) {
	m := astraModel(t, 80, 24)
	m.registerOperation(protocol.NewSubmitInput("new-command", "acceptance-root", "new-input", "", protocol.InputSteer), "message")
	m.associateInputOperationFromEvent(protocol.EventView{Kind: protocol.EventUserMessage, MessageID: protocol.InputMessageID("other-input"), TurnID: "1"})
	if m.operations["new-command"].TurnID != "" {
		t.Fatal("unrelated image input matched")
	}
	m.associateInputOperationFromEvent(protocol.EventView{Kind: protocol.EventUserMessage, MessageID: protocol.InputMessageID("new-input"), TurnID: "2", Text: "image locator"})
	m.finishInputOperations("2", protocol.TurnSucceeded, "")
	if m.operations["new-command"].Active() {
		t.Fatal("image submission still pending")
	}
}
