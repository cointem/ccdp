package agent

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"ccdp/internal/protocol"
	"ccdp/internal/sessionview"
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
	deadline := time.After(2 * time.Second)
drain:
	for {
		select {
		case update, ok := <-sub.Updates():
			if !ok {
				t.Fatal("observation ended before final status")
			}
			if update.Type == protocol.UpdateResyncRequired {
				t.Fatal("stream flooded subscriber")
			}
			if update.Event != nil && update.Event.Kind == protocol.EventReasoning {
				count++
				latest = update.Event.Transcript
			}
			if update.Event != nil && update.Event.Kind == protocol.EventStatus && update.Event.Text == "finished reasoning" {
				break drain
			}
		case <-deadline:
			t.Fatal("owner did not flush final stream before status")
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
	tr := &sessionview.Transcript{}
	ev := protocol.EventView{Kind: protocol.EventReasoning, TurnID: "1", Text: strings.Repeat("x", 32)}
	for i := 0; i < b.N; i++ {
		tr.Event(ev)
	}
}
