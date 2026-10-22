package agent

import (
	"strings"
	"testing"

	"ccdp/internal/session"
)

func TestExportSessionMarkdownPreservesProgramResults(t *testing.T) {
	var records []session.Record
	for i, status := range []string{"success", "denied", "unknown"} {
		callID := "child-" + status
		records = append(records, session.Record{Event: &session.ToolFinished{
			TurnID: "turn-1", StepID: "step-1", CallID: callID,
			ParentCallID: "parent-1", ChildIndex: i + 1, ResultTarget: "program", Status: status,
			Call:   &session.ToolCall{CallID: callID, ToolID: "Write"},
			Result: session.ToolResult{CallID: callID, Status: status, Text: "output-" + status},
		}})
	}
	snapshot, err := snapshotFromRecords(records, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.History) != 0 {
		t.Fatal("program results unexpectedly entered model history")
	}
	output := ExportSessionMarkdown(snapshot)
	for _, want := range []string{"Code Mode execution log", "Parent: `parent-1`", "output-success", "output-denied", "output-unknown"} {
		if !strings.Contains(output, want) {
			t.Fatalf("export lost %q:\n%s", want, output)
		}
	}
}
