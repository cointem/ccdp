package tui

import (
	"ccdp/internal/protocol"
	"context"
	"testing"
)

func statePointer(v protocol.SessionView) *protocol.SessionState { s := v.State(); return &s }

func TestLightStatePreservesCatalogAndUpdatesRuntimeFacts(t *testing.T) {
	client := &recordingClient{snapshot: protocolSnapshot("session", protocol.MessageView{ID: "m1", Role: "assistant", Content: "old"})}
	client.snapshot.Catalog.Version = 99
	m := NewWithClient(client, "", false)
	t.Cleanup(m.watchCancel)
	m.applySnapshot(client.snapshot)
	sub, err := client.Watch(context.Background(), protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	m.subscription = sub
	state := protocol.SessionState{SessionID: "session", Revision: client.snapshot.Revision, Phase: protocol.PhaseExecutingTools, Busy: true, HistoryLength: 30, OutstandingWork: 1, Transcript: client.snapshot.Transcript, Goal: &protocol.Goal{Status: protocol.GoalActive, Objective: "finish"}}
	next, _ := m.handleWatchUpdate(watchUpdateMsg{sub: sub, update: protocol.Update{Type: protocol.UpdateState, State: &state}, sessionID: "session", generation: m.watchGeneration})
	m = modelValue(t, next)
	if m.snapshot.Catalog.Version != 99 || len(m.snapshot.History) != 1 {
		t.Fatal("light state lost static snapshot fields")
	}
	if !m.busy || m.snapshot.HistoryLength != 30 || m.snapshot.OutstandingWork != 1 || m.snapshot.Goal == nil {
		t.Fatalf("runtime state was not applied: %+v", m.snapshot)
	}
}
