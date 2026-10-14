package sessionview

import (
	"testing"

	"ccdp/internal/protocol"
)

func TestHubSlowSubscriberEndsWithResync(t *testing.T) {
	var hub *Hub
	hub = NewHub("epoch", func(id uint64) { hub.Remove(id) })
	rev := protocol.Revision{LogSeq: 10, ViewGeneration: 2}
	sub := hub.Subscribe(protocol.Update{Type: protocol.UpdateSnapshot, Cursor: hub.Cursor(rev), Revision: rev})
	for i := 0; i < 1000; i++ {
		update := protocol.Update{Type: protocol.UpdateStream, Revision: rev}
		hub.Stamp(&update)
		hub.Publish(update)
	}
	count := 0
	var last protocol.Update
	for update := range sub.Updates() {
		count++
		last = update
	}
	if count != 64 || last.Type != protocol.UpdateResyncRequired {
		t.Fatalf("count=%d last=%s", count, last.Type)
	}
	if last.Cursor.StreamEpoch != "epoch" || last.Cursor.EventSeq != 63 {
		t.Fatalf("resync cursor=%+v", last.Cursor)
	}
	if err := sub.Close(); err != nil {
		t.Fatal(err)
	}
	if len(hub.subscribers) != 0 {
		t.Fatal("terminal subscriber retained")
	}
}

func TestHubOwnerCloseAndConsumerCloseAreIdempotent(t *testing.T) {
	var hub *Hub
	hub = NewHub("epoch", func(id uint64) { hub.Remove(id) })
	sub := hub.Subscribe(protocol.Update{})
	_ = sub.Close()
	_ = sub.Close()
	hub.Close()
	hub.Close()
	for range sub.Updates() {
	}
	next := hub.Subscribe(protocol.Update{})
	select {
	case <-next.Done():
	default:
		t.Fatal("closed hub admitted subscriber")
	}
	if _, open := <-next.Updates(); open {
		t.Fatal("closed hub retained an update channel")
	}
}

func TestHubIsolatesSubscribersAndPublisher(t *testing.T) {
	hub := NewHub("epoch", func(uint64) {})
	first := hub.Subscribe(protocol.Update{})
	second := hub.Subscribe(protocol.Update{})
	<-first.Updates()
	<-second.Updates()
	view := protocol.SessionView{History: []protocol.MessageView{{Content: "original"}}, Catalog: protocol.CatalogSnapshot{Tools: []protocol.ToolSnapshot{{ID: "Read", Schema: []byte(`{"type":"object"}`)}}}}
	update := protocol.Update{Type: protocol.UpdateSnapshot, Snapshot: &view, Event: &protocol.EventView{Tool: &protocol.ToolView{Args: []byte(`{}`)}}}
	hub.Publish(update)
	one, two := <-first.Updates(), <-second.Updates()
	one.Snapshot.History[0].Content = "caller"
	one.Snapshot.Catalog.Tools[0].Schema[0] = '!'
	one.Event.Tool.Args[0] = '!'
	if two.Snapshot.History[0].Content != "original" || view.History[0].Content != "original" || two.Snapshot.Catalog.Tools[0].Schema[0] == '!' || two.Event.Tool.Args[0] == '!' {
		t.Fatal("subscription values share writable storage")
	}
	view.History[0].Content = "publisher"
	if two.Snapshot.History[0].Content != "original" {
		t.Fatal("publisher retained subscriber storage")
	}
}
