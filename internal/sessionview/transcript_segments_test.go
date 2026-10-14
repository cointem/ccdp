package sessionview

import (
	"reflect"
	"testing"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func TestSegmentedTranscriptCommitAndReplayAgree(t *testing.T) {
	live := &Transcript{}
	segments := []messages.StreamSegment{{Kind: "thinking", Bytes: 3}, {Kind: "assistant", Bytes: 3}, {Kind: "thinking", Bytes: 3}, {Kind: "assistant", Bytes: 3}}
	for _, e := range []protocol.EventView{
		{Kind: protocol.EventReasoning, Text: "one"}, {Kind: protocol.EventStream, Text: "two"},
		{Kind: protocol.EventReasoning, Text: "six"}, {Kind: protocol.EventStream, Text: "ten"},
	} {
		e.TurnID = "1"
		live.Event(e)
	}
	if len(live.items) != 4 {
		t.Fatalf("live segments: %+v", live.items)
	}
	fact := session.AssistantCommitted{TurnID: "1", Message: session.Message{MessageID: "answer", Role: "assistant", ReasoningContent: "onesix", StreamSegments: segments, Content: []session.ContentBlock{{Kind: session.ContentText, Text: "twoten"}}}}
	live.Facts([]session.Event{fact})
	replay := &Transcript{}
	replay.Facts([]session.Event{fact})
	got, _ := live.Snapshot()
	want, _ := replay.Snapshot()
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
