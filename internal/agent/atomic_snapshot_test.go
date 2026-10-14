package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func TestSnapshotReadsPublishedStateWithoutOwnerLock(t *testing.T) {
	a := newRuntimeAgent(t)
	if err := a.appendHistory(messages.Message{Role: messages.RoleAssistant, Content: "published", ToolCalls: []messages.ToolCall{{ID: "call", Name: "Read", Arguments: map[string]any{}}}}); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	done := make(chan protocol.SessionView, 1)
	go func() { view, _ := a.Snapshot(context.Background()); done <- view }()
	var first protocol.SessionView
	select {
	case first = <-done:
	case <-time.After(time.Second):
		a.mu.Unlock()
		t.Fatal("Snapshot waited for owner lock")
	}
	a.mu.Unlock()
	if len(first.History) != 1 || first.History[0].Content != "published" {
		t.Fatal("acknowledgment preceded snapshot publication")
	}
	first.History[0].Content = "changed"
	first.History[0].ToolCalls[0].Name = "changed"
	first.Catalog.Tools[0].Schema[0] = '!'
	second, err := a.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.History[0].Content != "published" || second.History[0].ToolCalls[0].Name != "Read" || second.Catalog.Tools[0].Schema[0] == '!' {
		t.Fatal("reader changed published state")
	}
}

// Pause after the Store has committed but before the Writer has acknowledged
// it. Readers must retain the previous complete view and cancellation must not
// be coupled to the journal mutex through a stream producer's runtime lock.
func TestBlockedCommitKeepsSnapshotAndCloseResponsive(t *testing.T) {
	a := newRuntimeAgent(t)
	if err := a.appendHistory(messages.Message{Role: messages.RoleAssistant, Content: "confirmed"}); err != nil {
		t.Fatal(err)
	}
	before, err := a.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	b := journalBackend(t, a.persistenceHandle())
	b.mu.Lock()
	b.store = &admissionBoundaryStore{Store: b.store, afterCommit: func(kind session.EventType) {
		if kind == session.EventTypeAssistantCommitted {
			close(entered)
			<-release
		}
	}}
	b.mu.Unlock()
	completed := make(chan error, 1)
	go func() {
		completed <- a.appendHistory(messages.Message{Role: messages.RoleAssistant, Content: "awaiting acknowledgment"})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("commit did not reach publication barrier")
	}
	view, err := a.Snapshot(context.Background())
	if err != nil || view.Revision != before.Revision || len(view.History) != 1 || view.History[0].Content != "confirmed" {
		t.Fatalf("reader observed a partially installed commit: %+v, %v", view, err)
	}
	previewDone := make(chan struct{})
	go func() {
		a.emitStatus("progress during commit")
		close(previewDone)
	}()
	select {
	case <-previewDone:
	case <-time.After(time.Second):
		t.Fatal("event publication waited on the journal while holding the runtime lock")
	}
	closed := make(chan error, 1)
	go func() { closed <- a.CloseContext(context.Background()) }()
	select {
	case <-a.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("Close could not cancel execution while commit was blocked")
	}
	select {
	case <-closed:
		t.Fatal("Close released resources before the admitted commit settled")
	default:
	}
	once.Do(func() { close(release) })
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not drain the settled commit")
	}
	after, err := a.Snapshot(context.Background())
	if err != nil || len(after.History) != 2 || after.Revision.LogSeq <= before.Revision.LogSeq || after.Phase != protocol.PhaseClosed {
		t.Fatalf("final snapshot lost committed history: %+v, %v", after, err)
	}
}

func TestStopCancelsCapturedTurnWhileCommitIsBlocked(t *testing.T) {
	a := newRuntimeAgent(t)
	turn, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.mu.Lock()
	a.turn.turnCtx, a.turn.turnCancel = turn, cancel
	a.mu.Unlock()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	backend := journalBackend(t, a.persistenceHandle())
	backend.mu.Lock()
	backend.store = &admissionBoundaryStore{Store: backend.store, afterCommit: func(kind session.EventType) {
		if kind == session.EventTypeAssistantCommitted {
			close(entered)
			<-release
		}
	}}
	backend.mu.Unlock()
	appendDone := make(chan error, 1)
	go func() {
		appendDone <- a.appendHistory(messages.Message{Role: messages.RoleAssistant, Content: "commit in progress"})
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("commit did not block")
	}
	stopDone := make(chan error, 1)
	go func() {
		receipt, err := a.Submit(context.Background(), protocol.Command{ID: "blocked-stop", SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandStop})
		if err == nil && receipt.Rejected() {
			err = receipt.Error
		}
		stopDone <- err
	}()
	select {
	case <-turn.Done():
	case <-time.After(time.Second):
		t.Fatal("Stop waited for fsync before cancelling execution")
	}
	once.Do(func() { close(release) })
	if err := <-appendDone; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop receipt was lost after commit")
	}
}
