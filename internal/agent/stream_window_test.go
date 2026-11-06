package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"ccdp/internal/protocol"
	"ccdp/internal/sessionview"
)

func TestDeferredToolEventOwnsPayload(t *testing.T) {
	a, events := newTestAgent(t, &fakeLLM{})
	a.mu.Lock()
	a.publications.deferToolEvents = true
	a.mu.Unlock()
	event := protocol.EventView{Kind: protocol.EventToolResult, TurnID: "original-turn", StepID: "original-step", Tool: &protocol.ToolView{
		ID: "call", Name: "Read", Status: "error", Args: json.RawMessage(`{"path":"original"}`),
		Failure: &protocol.ToolFailure{Code: "original"},
	}}
	a.emit(event)
	event.Tool.ID = "changed"
	event.Tool.Args[0] = '!'
	event.Tool.Failure.Code = "changed"
	a.mu.Lock()
	pending := a.publications.deferredToolEvents
	a.publications.deferredToolEvents = nil
	a.publications.deferToolEvents = false
	a.mu.Unlock()
	if len(pending) != 1 {
		t.Fatalf("deferred events: %d", len(pending))
	}
	a.emit(pending[0])
	seen := drainUntil(t, events, time.Second, func(ev protocol.EventView) bool { return ev.Kind == protocol.EventToolResult })
	got := seen[len(seen)-1]
	if got.CallID != "call" || got.TurnID != "original-turn" || got.StepID != "original-step" || string(got.Tool.Args) != `{"path":"original"}` || got.Tool.Failure.Code != "original" {
		t.Fatalf("producer mutation reached deferred publication: %+v", got)
	}
}

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
		a.emit(protocol.EventView{Kind: protocol.EventReasoning, Text: text})
	}
	a.emit(protocol.EventView{Kind: protocol.EventReasoning, Text: "THE_FINAL_TAIL"})
	a.emit(protocol.EventView{Kind: protocol.EventStatus, Text: "finished reasoning"})
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
				// The queue bounds bytes as well as event count. A cropped batch
				// must end this observation and be replaced by a fresh snapshot.
				resumed, err := a.Watch(context.Background(), protocol.Cursor{})
				if err != nil {
					t.Fatal(err)
				}
				defer resumed.Close()
				initial := <-resumed.Updates()
				if initial.Snapshot == nil {
					t.Fatal("missing resync snapshot")
				}
				for _, item := range initial.Snapshot.Transcript {
					if item.Kind == "thinking" {
						copy := item
						latest = &copy
					}
				}
				break drain
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
