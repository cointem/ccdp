package agent

import (
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"fmt"
	"strings"
	"testing"
)

func TestForwardedReviewReasoningReconcilesFinalSnapshot(t *testing.T) {
	tr := &transcriptState{}
	for i := 0; i < 20; i++ {
		tr.put(protocol.TranscriptItem{ID: fmt.Sprintf("review:live:%d", i), Kind: "thinking", Text: "partial", Status: "completed"})
		tr.put(protocol.TranscriptItem{ID: fmt.Sprintf("review:tool:%d", i), Kind: "tool", Status: "success"})
	}
	for repeat := 0; repeat < 2; repeat++ {
		for i := 0; i < 20; i++ {
			tr.put(protocol.TranscriptItem{ID: fmt.Sprintf("review:saved:%d", i), PreviousID: fmt.Sprintf("review:live:%d", i), Kind: "thinking", Text: "complete thought", Status: "completed"})
		}
	}
	if len(tr.items) != 40 {
		t.Fatalf("duplicate thoughts: %d", len(tr.items))
	}
	for i := 0; i < 20; i++ {
		if tr.items[2*i].ID != fmt.Sprintf("review:saved:%d", i) || tr.items[2*i+1].Kind != "tool" {
			t.Fatal("final snapshot moved thoughts after tools")
		}
	}
}

func TestEmptyReasoningDeltaDoesNotCreateThought(t *testing.T) {
	tr := &transcriptState{}
	tr.event(protocol.EventView{Kind: protocol.EventReasoning, TurnID: "1"})
	if len(tr.items) != 0 {
		t.Fatal("empty reasoning created a visible item")
	}
}

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

func TestReasoningProjectionContinuesPastLegacyLimit(t *testing.T) {
	transcript := &transcriptState{}
	ev := protocol.EventView{Kind: protocol.EventReasoning, TurnID: "1", Text: strings.Repeat("a", 16<<10)}
	transcript.event(ev)
	ev.Text = "still streaming"
	transcript.event(ev)
	item := transcript.eventItem(ev)
	if item == nil || !strings.HasSuffix(item.Text, ev.Text) || item.Truncated {
		t.Fatalf("reasoning stopped at legacy 16 KiB limit: %#v", item)
	}

	ev.Text = strings.Repeat("b", transcriptTextLimit)
	transcript.event(ev)
	item = transcript.eventItem(ev)
	if item == nil || len(item.Text) != transcriptTextLimit || !item.Truncated {
		t.Fatalf("reasoning did not stop at the new bounded limit: len=%d item=%#v", len(item.Text), item)
	}
	ev.Text = "最新思考内容"
	transcript.event(ev)
	item = transcript.eventItem(ev)
	if !strings.HasSuffix(item.Text, ev.Text) || item.TextOffset == 0 {
		t.Fatal("bounded stream froze", item.TextOffset)
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

func TestReasoningInterleavedWithAnswerStartsNewSegment(t *testing.T) {
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
	if item.ID == id || item.Text != "." || item.Status != "streaming" || len(transcript.items) != 3 || transcript.items[0].Kind != "thinking" {
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

func TestTurnDurationFollowsFinalAnswerOnReplay(t *testing.T) {
	facts := []session.Event{
		session.AssistantCommitted{TurnID: "turn-7", StepID: "step-1", Message: session.Message{
			MessageID: "answer-7", Role: "assistant", Content: []session.ContentBlock{{Kind: session.ContentText, Text: "done"}},
		}},
		session.TurnFinished{TurnID: "turn-7", Outcome: "success", DurationMs: 155_000},
	}
	for _, events := range [][]session.Event{facts, {&session.AssistantCommitted{TurnID: "turn-7", StepID: "step-1", Message: session.Message{
		MessageID: "answer-7", Role: "assistant", Content: []session.ContentBlock{{Kind: session.ContentText, Text: "done"}},
	}}, &session.TurnFinished{TurnID: "turn-7", Outcome: "success", DurationMs: 155_000}}} {
		tr := &transcriptState{}
		tr.facts(events)
		items, _ := tr.snapshot()
		if len(items) != 2 || items[0].ID != "answer-7" || items[1].ID != "turn-summary:turn-7" || items[1].DurationMs != 155_000 {
			t.Fatalf("turn duration did not follow final answer: %+v", items)
		}
	}
}
