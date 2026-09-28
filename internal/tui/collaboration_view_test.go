package tui

import (
	"strings"
	"testing"

	"ccdp/internal/protocol"
)

func TestCollaborationCellIsNotUserOrRawJSON(t *testing.T) {
	for _, kind := range []string{"message", "final_result"} {
		text := protocol.EncodeCollaboration(protocol.CollaborationMessage{Kind: kind, AgentID: "child", Name: "researcher", Text: "report body", Outcome: "succeeded"})
		cell := historyCell{kind: "collaboration", text: text}
		out := renderItemWidth(&cell, 100)
		if strings.Contains(out, "Agent collaboration data") || strings.Contains(out, `"agent_id"`) || strings.Contains(out, "❯") {
			t.Fatalf("internal envelope rendered as user data: %s", out)
		}
		if !strings.Contains(out, "researcher") {
			t.Fatalf("missing source: %s", out)
		}
		if kind == "message" && !strings.Contains(out, "report body") {
			t.Fatal("progress message hidden")
		}
		if kind == "final_result" && (!strings.Contains(out, "结果已交付") || strings.Contains(out, "report body")) {
			t.Fatal("completion should be compact, with body in child details")
		}
	}
}
