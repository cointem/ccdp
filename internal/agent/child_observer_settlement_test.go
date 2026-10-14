package agent

import (
	"context"
	"testing"
	"time"

	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func TestClosedRuntimeSnapshotKeepsDurableRevision(t *testing.T) {
	a := newRuntimeAgent(t)
	for _, id := range []protocol.InputID{"mail-1", "mail-2", "mail-3"} {
		r := a.applySubmitInput(protocol.NewSubmitInput(protocol.CommandID(id), "", id, "progress", protocol.InputMessage))
		if r.Rejected() {
			t.Fatal(r.Error)
		}
	}
	before, err := a.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := a.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision.LogSeq < before.Revision.LogSeq {
		t.Fatalf("closed snapshot rolled revision backward: before=%+v after=%+v", before.Revision, after.Revision)
	}
}

func TestChildWatchPublishesIdleOnRuntimeDetach(t *testing.T) {
	a := newRuntimeAgent(t)
	a.mu.Lock()
	a.lifecycle.phase = protocol.PhaseStopping
	a.mu.Unlock()
	id := protocol.SessionID(a.conversation.sessionID)
	r := &managedRun{agent: a, fact: session.ChildRunRecorded{Child: protocol.ChildSession{SessionID: id, Run: protocol.RunView{ID: "run", Status: "succeeded"}}}}
	s := testSupervisor(a, r)
	c := &managedClient{supervisor: s, run: r, id: id}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sub, err := c.Watch(ctx, protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	// The initial snapshot precedes observer attachment. Wait for its own
	// full snapshot so it has recorded the terminal run status and instance.
	count := 0
	for count < 2 {
		select {
		case u := <-sub.Updates():
			if u.Type == protocol.UpdateSnapshot && u.Snapshot != nil && u.Snapshot.Phase == protocol.PhaseStopping {
				count++
			}
		case <-ctx.Done():
			t.Fatal("observer did not attach")
		}
	}
	r.mu.Lock()
	r.final = protocol.SessionView{SessionID: id, Phase: protocol.PhaseClosed, Transcript: []protocol.TranscriptItem{}}
	r.agent = nil // same Run.ID and succeeded status, only ownership changes
	r.mu.Unlock()
	for {
		select {
		case u := <-sub.Updates():
			if u.Type == protocol.UpdateSnapshot && u.Snapshot != nil && u.Snapshot.Phase == protocol.PhaseIdle && !u.Snapshot.Closing && !u.Snapshot.Busy {
				return
			}
		case <-ctx.Done():
			t.Fatal("child watch retained stopping after instance detached")
		}
	}
}
