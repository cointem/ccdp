package agent

import (
	"testing"

	"ccdp/internal/messages"
)

func TestCompactBoundaryKeepsWholeMultiCallGroup(t *testing.T) {
	history := []messages.Message{
		{Role: messages.RoleUser, Content: "task"},
		{Role: messages.RoleAssistant, ToolCalls: []messages.ToolCall{{ID: "a"}, {ID: "b"}}},
		{Role: messages.RoleTool, ToolCallID: "a"},
		{Role: messages.RoleTool, ToolCallID: "b"},
		{Role: messages.RoleAssistant, Content: "done"},
	}
	if cut := compactCut(history, 2, 10000); cut != 1 {
		t.Fatalf("split tool group at %d", cut)
	}
	if cut := compactCut(history, 1, 10000); cut != 4 {
		t.Fatalf("complete group boundary=%d", cut)
	}
	history = history[:3] // b has no result; it cannot be summarized away.
	if cut := compactCut(history, 1, 0); cut != 1 {
		t.Fatalf("lost unfinished group at %d", cut)
	}
}
