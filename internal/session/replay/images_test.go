package replay

import (
	"strings"
	"testing"

	"ccdp/internal/messages"
	"ccdp/internal/session"
)

func TestToolImageProjectionAndCrashRecovery(t *testing.T) {
	for _, projected := range []bool{false, true} {
		p := New()
		message, err := session.EncodeMessage(messages.Message{ID: "assistant", Role: messages.RoleAssistant, ToolCalls: []messages.ToolCall{{ID: "screen", Name: "Computer", Arguments: map[string]any{}}, {ID: "read", Name: "Read", Arguments: map[string]any{}}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Apply(session.AssistantCommitted{TurnID: "t", StepID: "s", Message: message}); err != nil {
			t.Fatal(err)
		}
		image := &session.BlobRef{Hash: strings.Repeat("a", 64), Size: 123, MediaType: "image/jpeg"}
		for i, id := range []string{"screen", "read"} {
			start := session.ToolStarted{TurnID: "t", StepID: "s", CallIndex: i, Call: session.ToolCall{CallID: id, ToolID: "test", Arguments: []byte(`{}`)}}
			finish := session.ToolFinished{TurnID: "t", StepID: "s", CallID: id, Status: "success", Result: session.ToolResult{CallID: id, Status: "success", Text: "done"}}
			if i == 0 {
				finish.Image = image
			}
			for _, e := range []session.Event{start, finish} {
				if err := p.Apply(e); err != nil {
					t.Fatal(err)
				}
			}
		}
		image.Hash = "mutated"
		if projected {
			if err := p.Apply(session.ToolResultsProjected{TurnID: "t", StepID: "s", Results: []session.ProjectedToolResult{{CallID: "screen", Status: "success", Text: "done"}, {CallID: "read", Status: "success", Text: "done"}}}); err != nil {
				t.Fatal(err)
			}
		}
		p.FinalizeRecovery()
		p.FinalizeRecovery()
		if len(p.History) != 4 || p.History[1].Role != messages.RoleTool || p.History[2].Role != messages.RoleTool {
			t.Fatalf("image interrupted result pairing (projected=%t): %+v", projected, p.History)
		}
		img := p.History[3]
		if img.InternalSource != "runtime_computer" || !img.ImagesFrozen || img.ImageAttachments[0].BlobHash != strings.Repeat("a", 64) {
			t.Fatalf("lost image: %+v", img)
		}
		if len(p.historyStep) != len(p.History) {
			t.Fatal("history identities out of sync")
		}
		q := p.Clone()
		q.History[3].ImageAttachments[0].BlobHash = "changed"
		if p.History[3].ImageAttachments[0].BlobHash == "changed" {
			t.Fatal("image aliases replay clone")
		}
	}
}
