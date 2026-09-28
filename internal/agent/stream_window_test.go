package agent

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"ccdp/internal/protocol"
)

func TestStreamPublicationCoalescesAndFlushesFinalTail(t *testing.T) {
	a, _ := newTestAgent(t, &fakeLLM{})
	sub, err := a.Watch(context.Background(), protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	<-sub.Updates()
	text := strings.Repeat("中文", 100)
	for i := 0; i < 2000; i++ {
		a.publishEvent(Event{Type: EventReasoning, Text: text})
	}
	a.publishEvent(Event{Type: EventReasoning, Text: "THE_FINAL_TAIL"})
	a.publishEvent(Event{Type: EventStatus, Text: "finished reasoning"})
	count := 0
	var latest *protocol.TranscriptItem
drain:
	for {
		select {
		case update := <-sub.Updates():
			if update.Type == protocol.UpdateResyncRequired {
				t.Fatal("stream flooded subscriber")
			}
			if update.Event != nil && update.Event.Kind == protocol.EventReasoning {
				count++
				latest = update.Event.Transcript
			}
		default:
			break drain
		}
	}
	if count > 20 {
		t.Fatal("uncoalesced stream publications", count)
	}
	if latest == nil || !strings.HasSuffix(latest.Text, "THE_FINAL_TAIL") || !utf8.ValidString(latest.Text) || latest.TextOffset == 0 {
		t.Fatal("missing latest bounded window")
	}
}

func BenchmarkLongReasoningWindow(b *testing.B) {
	tr := &transcriptState{}
	ev := protocol.EventView{Kind: protocol.EventReasoning, TurnID: "1", Text: strings.Repeat("x", 32)}
	for i := 0; i < b.N; i++ {
		tr.event(ev)
	}
}
