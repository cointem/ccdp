package sessionview

import (
	"context"
	"testing"

	"ccdp/internal/session"
)

func TestRecoveredQueuedAndRunningToolsKeepCompletedResults(t *testing.T) {
	call := session.ToolCall{CallID: "queued", ToolID: "Bash", Arguments: []byte(`{}`)}
	records := []session.Record{
		{Seq: 1, Event: session.AssistantCommitted{TurnID: "old", StepID: "step", Message: session.Message{
			MessageID: "message", Role: "assistant", Content: []session.ContentBlock{{Kind: session.ContentToolCall, ToolCall: &call}}}}},
		{Seq: 2, Event: session.ToolStarted{TurnID: "old", StepID: "step", Call: session.ToolCall{CallID: "done", ToolID: "Read"}}},
		{Seq: 3, Event: session.ToolFinished{TurnID: "old", StepID: "step", CallID: "done", Status: "success",
			Result: session.ToolResult{CallID: "done", Status: "success", Text: "completed output"}}},
		{Seq: 4, Event: session.ToolStarted{TurnID: "old", StepID: "step", Call: session.ToolCall{CallID: "code", ToolID: "RunCode"}}},
		{Seq: 5, Event: session.ToolStarted{TurnID: "old", StepID: "step", ParentCallID: "code", ChildIndex: 1,
			Call: session.ToolCall{CallID: "child", ToolID: "Write"}}},
	}
	tr := FromRecords(records)
	items, _ := tr.Snapshot()
	for _, item := range items {
		if item.CallID == "done" {
			if item.Status != "success" || item.Text != "completed output" {
				t.Fatalf("completed result changed: %+v", item)
			}
		} else if item.Status != "unknown" {
			t.Fatalf("unfinished recovered tool remains active: %+v", item)
		}
	}
	// A completion durably appended by the new writer supersedes unknown.
	records = append(records, session.Record{Seq: 6, Event: session.ToolFinished{TurnID: "old", StepID: "step", CallID: "queued",
		Status: "error", Result: session.ToolResult{CallID: "queued", Status: "error", Text: "late result"}}})
	page := NewTranscript(0, Window)
	if err := page.ReplayRecords(context.Background(), records, 5); err != nil {
		t.Fatal(err)
	}
	items, _ = page.Snapshot()
	if items[0].Status != "error" || items[0].Text != "late result" {
		t.Fatalf("new fact did not replace recovery projection: %+v", items[0])
	}
}
