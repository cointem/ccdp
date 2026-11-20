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

func TestRuntimeContextStaysOutOfVisibleTranscript(t *testing.T) {
	var messages []session.Message
	var facts []session.Event
	for _, input := range []struct{ id, source string }{
		{"user", ""}, {"access", "runtime_access"}, {"repository", "runtime_repository"}, {"answer", ""},
	} {
		message := session.Message{MessageID: input.id, Role: "user", InternalSource: input.source,
			Content: []session.ContentBlock{{Kind: session.ContentText, Text: `<runtime_context section="access">example</runtime_context>`}}}
		if input.id == "answer" {
			message.Role = "assistant"
		}
		messages = append(messages, message)
		facts = append(facts, session.AssistantCommitted{TurnID: "turn-1", Message: message})
	}
	for name, events := range map[string][]session.Event{
		"append": facts,
		"reset":  {session.ConversationReset{TurnID: "turn-1", Messages: messages}},
	} {
		t.Run(name, func(t *testing.T) {
			live := &Transcript{window: 2}
			live.Facts(events)
			visible, more := live.Snapshot()
			if more || len(visible) != 2 || visible[0].ID != "user" || visible[1].ID != "answer" {
				t.Fatalf("internal context leaked or displaced user content: %+v, more=%v", visible, more)
			}
			var records []session.Record
			for i, event := range events {
				records = append(records, session.Record{Seq: uint64(i + 1), Event: event})
			}
			page := NewTranscript(0, 2)
			if err := page.ReplayRecords(context.Background(), records, session.Beginning); err != nil {
				t.Fatal(err)
			}
			replayed, more := page.Snapshot()
			if more || !reflect.DeepEqual(replayed, visible) || page.Position("answer") != 2 {
				t.Fatalf("replay or pagination exposed internal context: %+v", replayed)
			}
		})
	}
}

func TestWindowKeepsPromptOutsidePageOrdinals(t *testing.T) {
	var events []session.Event
	for _, id := range []string{"question", "progress1", "progress2", "answer"} {
		role := "assistant"
		if id == "question" {
			role = "user"
		}
		events = append(events, session.AssistantCommitted{TurnID: "turn-7", Message: session.Message{MessageID: id, Role: role, Content: []session.ContentBlock{{Kind: session.ContentText, Text: id}}}})
	}
	var records []session.Record
	for i, event := range events {
		records = append(records, session.Record{Seq: uint64(i + 1), Event: event})
	}
	live := &Transcript{window: 2}
	live.Facts(events)
	page := NewTranscript(0, 2)
	if err := page.ReplayRecords(context.Background(), records, session.Beginning); err != nil {
		t.Fatal(err)
	}
	for _, tr := range []*Transcript{live, page} {
		items, more, prompt := tr.SnapshotWithPrompt()
		if !more || len(items) != 2 || items[0].ID != "progress2" || items[1].ID != "answer" || prompt == nil || prompt.ID != "question" || prompt.Text != "question" {
			t.Fatalf("lost page or prompt: %+v %+v", items, prompt)
		}
		prompt.Text = "changed by reader"
		_, _, again := tr.SnapshotWithPrompt()
		if again.Text != "question" {
			t.Fatal("reader owned the prompt in the transcript")
		}
	}
	if page.Position("progress2") != 3 {
		t.Fatal("the leading prompt altered pagination")
	}
	live.Facts([]session.Event{session.ConversationReset{TurnID: "turn-8"}})
	if _, _, p := live.SnapshotWithPrompt(); p != nil {
		t.Fatal("reset retained a previous prompt")
	}
}

func TestAgentMailSurvivesForkWithoutBecomingUserInput(t *testing.T) {
	progress := protocol.EncodeCollaboration(protocol.CollaborationMessage{Kind: "message", AgentID: "worker", Text: "progress"})
	final := protocol.EncodeCollaboration(protocol.CollaborationMessage{Kind: "final_result", AgentID: "worker", Text: "result"})
	const original = "original"
	facts := []session.Event{
		session.InputQueued{InputID: original, Text: progress, Strategy: string(protocol.InputMessage)},
		session.InputDelivered{InputID: original, TurnID: "t"},
	}
	for _, c := range []struct{ id, source, text string }{
		{"copied", protocol.CollaborationSource, progress},
		{"report", protocol.CollaborationSource, final},
		{protocol.InputMessageID("agent-tool-quoted"), "", progress},
	} {
		facts = append(facts, session.AssistantCommitted{TurnID: "t", Message: session.Message{
			MessageID: c.id, Role: "user", InternalSource: c.source,
			Content: []session.ContentBlock{{Kind: session.ContentText, Text: c.text}},
		}})
	}
	tr := NewTranscript(0, 20)
	tr.Facts(facts)
	raw, _ := tr.Snapshot()
	if len(raw) != 4 || raw[0].Kind != "collaboration" || raw[0].Text != progress ||
		raw[1].Kind != "collaboration" || raw[1].Text != progress {
		t.Fatalf("lost mailbox history: %+v", raw)
	}
	visible := protocol.ChatItems(raw)
	if len(visible) != 2 || visible[0].Kind != "collaboration" || visible[0].Text != final ||
		visible[1].Kind != "user" || visible[1].Text != progress {
		t.Fatalf("incorrect chat projection: %+v", visible)
	}
}
