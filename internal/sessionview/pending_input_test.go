package sessionview

import (
	"context"
	"reflect"
	"testing"

	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func TestPendingUserInputEntersTranscriptAtConsumption(t *testing.T) {
	for _, strategy := range []protocol.InputStrategy{protocol.InputFollowup, protocol.InputSteer} {
		t.Run(string(strategy), func(t *testing.T) {
			messageID := protocol.InputMessageID("next")
			commit := func(id, role, turn, text string) session.Event {
				return session.AssistantCommitted{TurnID: turn, Message: session.Message{
					MessageID: id, Role: role, Content: []session.ContentBlock{{Kind: session.ContentText, Text: text}},
				}}
			}
			facts := []session.Event{
				commit("first", "user", "turn-1", "first request"),
				session.InputQueued{InputID: "next", MessageID: messageID, Text: "next request", Strategy: string(strategy)},
				commit("answer", "assistant", "turn-1", "previous answer"),
				session.InputDelivered{InputID: "next", TurnID: "turn-2"},
			}
			live := &Transcript{}
			live.Facts(facts[:3])
			before, _ := live.Snapshot()
			if len(before) != 2 || before[1].ID != "answer" {
				t.Fatalf("queued input leaked into transcript: %+v", before)
			}
			live.Facts(facts[3:])
			got, _ := live.Snapshot()
			if len(got) != 3 || got[2].ID != messageID || got[2].TurnID != "turn-2" {
				t.Fatalf("input did not follow previous answer: %+v", got)
			}
			var records []session.Record
			for i, fact := range facts {
				records = append(records, session.Record{Seq: session.Cursor(i + 1), Event: fact})
			}
			page := NewTranscript(0, 10)
			if err := page.ReplayRecords(context.Background(), records, session.Beginning); err != nil {
				t.Fatal(err)
			}
			replayed, _ := page.Snapshot()
			if !reflect.DeepEqual(got, replayed) || page.Position(messageID) != 3 {
				t.Fatalf("replay/pagination used admission order: %+v", replayed)
			}
		})
	}
}

func TestCancelledPendingUserInputNeverEntersTranscript(t *testing.T) {
	projection := &Transcript{}
	projection.Facts([]session.Event{
		session.InputQueued{InputID: "cancelled", Text: "unsent", Strategy: string(protocol.InputFollowup)},
		session.InputCancelled{InputID: "cancelled"},
		session.InputDelivered{InputID: "cancelled", TurnID: "turn-2"},
	})
	if items, _ := projection.Snapshot(); len(items) != 0 {
		t.Fatalf("unsent input entered transcript: %+v", items)
	}
}
