package sessionview

import (
	"ccdp/internal/session"
	"testing"
)

func TestCodeModeRecoveredChildrenAreUnknown(t *testing.T) {
	start := session.ToolStarted{TurnID: "turn", StepID: "step", ParentCallID: "parent", ChildIndex: 1, ResultTarget: "program", Call: session.ToolCall{CallID: "child", ToolID: "Write", Arguments: []byte(`{}`)}}
	transcript := FromRecords([]session.Record{{Event: &start}})
	items, _ := transcript.Snapshot()
	if len(items) != 1 || items[0].Status != "unknown" || items[0].ParentCallID != "parent" {
		t.Fatalf("%+v", items)
	}
	finish := session.ToolFinished{TurnID: "turn", StepID: "step", CallID: "child", ParentCallID: "parent", ChildIndex: 1, ResultTarget: "program", Status: "success", Result: session.ToolResult{CallID: "child", Status: "success", Text: "written"}}
	transcript = FromRecords([]session.Record{{Event: &start}, {Event: &finish}})
	items, _ = transcript.Snapshot()
	if items[0].Status != "success" {
		t.Fatalf("%+v", items)
	}
}
