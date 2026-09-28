package agent

import (
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"reflect"
	"testing"
)

func TestSegmentedTranscriptCommitAndReplayAgree(t *testing.T) {
	live := &transcriptState{}
	segments := []messages.StreamSegment{{Kind: "thinking", Bytes: 3}, {Kind: "assistant", Bytes: 3}, {Kind: "thinking", Bytes: 3}, {Kind: "assistant", Bytes: 3}}
	for _, e := range []protocol.EventView{
		{Kind: protocol.EventReasoning, Text: "one"}, {Kind: protocol.EventStream, Text: "two"},
		{Kind: protocol.EventReasoning, Text: "six"}, {Kind: protocol.EventStream, Text: "ten"},
	} {
		e.TurnID = "1"
		live.event(e)
	}
	if len(live.items) != 4 {
		t.Fatalf("live segments: %+v", live.items)
	}
	fact := session.AssistantCommitted{TurnID: "1", Message: session.Message{MessageID: "answer", Role: "assistant", ReasoningContent: "onesix", StreamSegments: segments, Content: []session.ContentBlock{{Kind: session.ContentText, Text: "twoten"}}}}
	live.facts([]session.Event{fact})
	replay := &transcriptState{}
	replay.facts([]session.Event{fact})
	got, _ := live.snapshot()
	want, _ := replay.snapshot()
	for i := range got {
		got[i].PreviousID = ""
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("live=%+v replay=%+v", got, want)
	}
	if len(got) != 4 {
		t.Fatalf("duplicated aggregate: %+v", got)
	}
}

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
