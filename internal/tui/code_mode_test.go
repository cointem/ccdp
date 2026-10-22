package tui

import (
	"ccdp/internal/protocol"
	"strings"
	"testing"
)

func TestCodeModeParentDetailsIncludeChildren(t *testing.T) {
	parent := historyCell{kind: "tool", toolID: "cell", toolName: "RunCode", turnID: "turn-1", status: "success", toolArgs: map[string]any{"description": "Summarize files"}}
	child := historyCell{kind: "tool", toolID: "read", parentCallID: "cell", childIndex: 1, toolName: "Read", turnID: protocol.TurnID("turn-1"), status: "success", text: "child-output", toolArgs: map[string]any{"file_path": "a.txt"}}
	m := &Model{}
	m.width = 100
	m.items = []historyCell{parent, child}
	text := stripANSI(m.detailContent(&m.items[0]))
	if !strings.Contains(text, "Summarize files") || !strings.Contains(text, "child-output") || !strings.Contains(text, "↳ 1") {
		t.Fatal(text)
	}
	other := child
	other.turnID = "turn-2"
	other.text = "unrelated"
	m.items = append(m.items, other)
	if strings.Contains(stripANSI(m.detailContent(&m.items[0])), "unrelated") {
		t.Fatal("parent matched reused call ID from another turn")
	}
}
