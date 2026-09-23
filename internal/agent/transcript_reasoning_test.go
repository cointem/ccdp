package agent

import (
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"testing"
)

func TestReasoningProjectionIsCumulativeAndIdentified(t *testing.T) {
	transcript := &transcriptState{}
	ev := protocol.EventView{Kind: protocol.EventReasoning, TurnID: "1", Text: "first "}
	transcript.event(ev)
	first := transcript.eventItem(ev)
	ev.Text = "second"
	transcript.event(ev)
	second := transcript.eventItem(ev)
	if first == nil || second == nil || first.ID == "" || first.ID != second.ID || second.Text != "first second" || second.Status != "streaming" {
		t.Fatalf("bad reasoning projection: %#v %#v", first, second)
	}
	transcript.event(protocol.EventView{Kind: protocol.EventStream, TurnID: "1", Text: "answer"})
	for _, item := range transcript.items {
		if item.ID == first.ID && item.Status != "completed" {
			t.Fatal("reasoning remained mutable after answer")
		}
	}
	ev.TurnID = "2"
	transcript.event(ev)
	if next := transcript.eventItem(ev); next == nil || next.ID == first.ID {
		t.Fatal("new reasoning segment reused prior identity")
	}
}

// Anthropic closes a thinking block that is followed by a tool_use (no answer
// text) with an empty text delta. That close signal must project the reasoning
// cell it just finalized so the TUI stops the "Thinking…" spinner at the
// thinking boundary instead of waiting for a later snapshot.
func TestReasoningCloseSignalProjectsCompletedReasoning(t *testing.T) {
	transcript := &transcriptState{}
	reasoning := protocol.EventView{Kind: protocol.EventReasoning, TurnID: "1", Text: "plan the call"}
	transcript.event(reasoning)
	id := transcript.eventItem(reasoning).ID

	closeEv := protocol.EventView{Kind: protocol.EventStream, TurnID: "1", Text: ""}
	transcript.event(closeEv)
	got := transcript.eventItem(closeEv)
	if got == nil {
		t.Fatal("reasoning-close signal carried no projection; thinking spinner would linger")
	}
	if got.ID != id || got.Kind != "thinking" || got.Status != "completed" {
		t.Fatalf("reasoning-close projected wrong cell: %#v", got)
	}
}

func TestReasoningInterleavedWithAnswerKeepsOneSegment(t *testing.T) {
	transcript := &transcriptState{}
	reasoning := protocol.EventView{Kind: protocol.EventReasoning, TurnID: "1", Text: "Analyzing"}
	transcript.event(reasoning)
	id := transcript.eventItem(reasoning).ID
	transcript.event(protocol.EventView{Kind: protocol.EventStatus, TurnID: "1", Text: "working"})
	if transcript.eventItem(reasoning).Status != "streaming" {
		t.Fatal("status ended reasoning")
	}
	transcript.event(protocol.EventView{Kind: protocol.EventStream, TurnID: "1", Text: "Hello"})
	reasoning.Text = "."
	transcript.event(reasoning)
	item := transcript.eventItem(reasoning)
	if item.ID != id || item.Text != "Analyzing." || len(transcript.items) != 2 || transcript.items[0].Kind != "thinking" {
		t.Fatalf("interleaved reasoning created an out-of-order fragment: %+v", transcript.items)
	}
	transcript.event(protocol.EventView{Kind: protocol.EventToolStarted, TurnID: "1"})
	reasoning.Text = "Next step"
	transcript.event(reasoning)
	if transcript.eventItem(reasoning).ID == id {
		t.Fatal("new tool step reused old reasoning")
	}
}

func TestCommittedReasoningRestoresAndReconcilesLiveSegment(t *testing.T) {
	for _, live := range []bool{false, true} {
		tr := &transcriptState{}
		if live {
			tr.event(protocol.EventView{Kind: protocol.EventReasoning, TurnID: "turn", Text: "partial"})
		}
		tr.facts([]session.Event{session.AssistantCommitted{TurnID: "turn", Message: session.Message{MessageID: "answer", Role: "assistant", ReasoningContent: "complete thought"}}})
		items, _ := tr.snapshot()
		if len(items) != 1 || items[0].Kind != "thinking" || items[0].Text != "complete thought" || items[0].Status != "completed" {
			t.Fatalf("reasoning lost or duplicated: %+v", items)
		}
	}
}
