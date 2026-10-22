package main

import (
	"ccdp/internal/protocol"
	"context"
	"testing"
	"time"
)

type completedResyncClient struct {
	*headlessTestClient
	recovered protocol.SessionView
	final     protocol.SessionView
	snapshots int
}

func (c *completedResyncClient) Snapshot(context.Context) (protocol.SessionView, error) {
	c.snapshots++
	return c.final, nil
}
func (c *completedResyncClient) Watch(context.Context, protocol.Cursor) (protocol.Subscription, error) {
	sub := &headlessTestSubscription{ch: make(chan protocol.Update, 8)}
	sub.send(protocol.Update{Type: protocol.UpdateSnapshot, Snapshot: &c.recovered})
	return sub, nil
}
func TestHeadlessCompletedResyncDoesNotReadSecondSnapshot(t *testing.T) {
	recovered := protocol.SessionView{SessionID: "session", Phase: protocol.PhaseIdle, History: []protocol.MessageView{
		{ID: "old", Role: "assistant", Content: "old"},
		{ID: "input", Role: "user", Content: "question"},
		{ID: "answer", Role: "assistant", Content: "complete answer"},
	}}
	final := recovered
	final.History = []protocol.MessageView{{ID: "compacted", Role: "user", Content: "summary"}, {ID: "answer", Role: "assistant", Content: "complete answer"}}
	client := &completedResyncClient{headlessTestClient: &headlessTestClient{}, recovered: recovered, final: final}
	sub := &headlessTestSubscription{ch: make(chan protocol.Update, 8)}
	sub.send(protocol.Update{Type: protocol.UpdateResyncRequired})
	watch := &headlessWatch{client: client, sub: sub}
	defer watch.close()
	sink := turnSink{historyAnchor: "input"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := consumeHeadlessTurn(ctx, ctx, watch, "session", outText, &sink, 1, client)
	if err != nil || sink.text.String() != "complete answer" || client.snapshots != 0 {
		t.Fatalf("unexpected result: text=%q err=%v snapshots=%d", sink.text.String(), err, client.snapshots)
	}

}
