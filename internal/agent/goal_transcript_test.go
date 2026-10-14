package agent

import (
	"testing"

	"ccdp/internal/session"
	"ccdp/internal/sessionview"
)

func TestGoalContinuationTranscriptIsInternal(t *testing.T) {
	transcript := &sessionview.Transcript{}
	transcript.Facts([]session.Event{session.AssistantCommitted{
		TurnID: "turn-1",
		Message: session.Message{MessageID: "goal-input", Role: "user", InternalSource: "goal",
			Content: []session.ContentBlock{{Kind: session.ContentText, Text: "Continue the goal."}}},
	}})
	items, _ := transcript.Snapshot()
	if len(items) != 1 || items[0].Kind != "system" {
		t.Fatalf("internal continuation displayed as user input: %+v", items)
	}
}
