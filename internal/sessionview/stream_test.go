package sessionview

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"ccdp/internal/protocol"
)

func TestStreamQueuePreservesIdentityOrderAndCadence(t *testing.T) {
	var q StreamQueue
	now := time.Unix(100, 0)
	first := protocol.EventView{Kind: protocol.EventStream, TurnID: "turn", StepID: "step-1", Text: "a"}
	if delay := q.Add(first, now); delay != 0 {
		t.Fatalf("first preview delayed: %v", delay)
	}
	if got := q.Drain(now); len(got) != 1 || got[0].Text != "a" {
		t.Fatalf("first flush=%+v", got)
	}
	first.Text = "b"
	if delay := q.Add(first, now.Add(10*time.Millisecond)); delay != 40*time.Millisecond {
		t.Fatalf("coalescing delay=%v", delay)
	}
	second := first
	second.StepID, second.Text = "step-2", "new step"
	q.Add(second, now.Add(20*time.Millisecond))
	first.Text = "c"
	q.Add(first, now.Add(30*time.Millisecond))
	got := q.Drain(now.Add(50 * time.Millisecond))
	if len(got) != 2 || got[0].Text != "bc" || got[1].Text != "new step" {
		t.Fatalf("cross-step preview or completion-order reordering: %+v", got)
	}
	if got := q.Drain(now.Add(time.Second)); len(got) != 0 {
		t.Fatalf("replayed drained preview: %+v", got)
	}
}

func TestStreamQueueBoundsUnicodeTail(t *testing.T) {
	var q StreamQueue
	now := time.Unix(100, 0)
	q.Add(protocol.EventView{Kind: protocol.EventReasoning, Text: strings.Repeat("中", TextLimit)}, now)
	q.Add(protocol.EventView{Kind: protocol.EventReasoning, Text: "末尾"}, now)
	got := q.Drain(now)
	if len(got) != 1 || len(got[0].Text) > TextLimit || !utf8.ValidString(got[0].Text) || !strings.HasSuffix(got[0].Text, "末尾") {
		t.Fatal("coalesced preview lost its bounded UTF-8 tail")
	}
}
