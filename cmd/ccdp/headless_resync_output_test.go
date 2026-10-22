package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"ccdp/internal/protocol"
)

type resyncOutputClient struct {
	*headlessTestClient
	final protocol.SessionView
}

func (c *resyncOutputClient) Snapshot(context.Context) (protocol.SessionView, error) {
	return c.final, nil
}

func (c *resyncOutputClient) Watch(context.Context, protocol.Cursor) (protocol.Subscription, error) {
	sub := &headlessTestSubscription{ch: make(chan protocol.Update, 8)}
	// A busy reconnect exposes only a bounded transcript tail. The full
	// committed answer becomes available in Snapshot at the end of the turn.
	initial := protocol.SessionView{SessionID: "session", Busy: true, Phase: protocol.PhaseStreaming,
		Transcript: []protocol.TranscriptItem{{ID: "live", Kind: "assistant", Text: "bounded preview tail", Truncated: true}}}
	sub.send(protocol.Update{Type: protocol.UpdateSnapshot, Snapshot: &initial})
	sub.send(protocol.Update{Type: protocol.UpdateStream, Event: &protocol.EventView{
		SessionID: "session", Kind: protocol.EventStream, Text: "-tail",
	}})
	state := c.final.State()
	sub.send(protocol.Update{Type: protocol.UpdateState, State: &state})
	sub.send(protocol.Update{Type: protocol.UpdateStream, Event: &protocol.EventView{
		SessionID: "session", Kind: protocol.EventTurnDone, TurnID: "turn-1",
	}})
	return sub, nil
}

func TestHeadlessResyncRestoresFullCommittedOutput(t *testing.T) {
	for _, format := range []outputFormat{outText, outJSON, outStreamJSON} {
		t.Run(string(format), func(t *testing.T) {
			for _, missingAnchor := range []bool{false, true} {
				answer := "prefix-" + strings.Repeat("missed text ", 8000) + "-tail"
				final := protocol.SessionView{SessionID: "session", Phase: protocol.PhaseIdle,
					LastTurn: &protocol.TurnOutcome{TurnID: "turn-1", Status: protocol.TurnSucceeded},
					History: []protocol.MessageView{
						{ID: "old-answer", Role: "assistant", Content: "old answer must not reappear"},
						{ID: "input", Role: "user", Content: "new request"},
						{ID: "intermediate", Role: "assistant", Content: "before tool\n"},
						{ID: "answer", Role: "assistant", Content: answer},
					}}
				if missingAnchor {
					final.History[1].ID = "compacted"
				}
				client := &resyncOutputClient{headlessTestClient: &headlessTestClient{}, final: final}
				sub := &headlessTestSubscription{ch: make(chan protocol.Update, 8)}
				sub.send(protocol.Update{Type: protocol.UpdateStream, Event: &protocol.EventView{
					SessionID: "session", Kind: protocol.EventStream, Text: "prefix",
				}})
				sub.send(protocol.Update{Type: protocol.UpdateResyncRequired})
				watch := &headlessWatch{client: client, sub: sub}
				sink := turnSink{historyAnchor: "input"}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				err := consumeHeadlessTurn(ctx, ctx, watch, "session", format, &sink, 1, client)
				cancel()
				watch.close()
				if format == outStreamJSON {
					if err == nil || !strings.Contains(err.Error(), "stream resynchronized") {
						t.Fatalf("stream-json lost its explicit resync error: %v", err)
					}
					continue
				}
				if missingAnchor {
					if err == nil || !strings.Contains(err.Error(), "history boundary") {
						t.Fatalf("missing input boundary returned incomplete success: %v", err)
					}
					continue
				}
				if err != nil || sink.text.String() != "before tool\n"+answer {
					t.Fatalf("resync result err=%v bytes=%d want=%d", err, sink.text.Len(), len("before tool\n"+answer))
				}
			}
		})
	}
}
