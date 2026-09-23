package agent

import (
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"context"
	"testing"
	"time"
)

func TestChildWatchPublishesIdleOnRuntimeDetach(t *testing.T) {
	a := newRuntimeAgent(t)
	a.mu.Lock()
	a.phase = protocol.PhaseStopping
	a.mu.Unlock()
	id := protocol.SessionID(a.sessionID)
	r := &managedRun{agent: a, fact: session.ChildRunRecorded{Child: protocol.ChildSession{SessionID: id, Run: protocol.RunView{ID: "run", Status: "succeeded"}}}}
	s := &SessionSupervisor{root: a, children: map[protocol.SessionID]*managedRun{id: r}}
	c := &managedClient{supervisor: s, run: r, id: id}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sub, err := c.Watch(ctx, protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	// The initial snapshot precedes observer attachment. Wait for its own
	// state snapshot so it has recorded the terminal run status and instance.
	count := 0
	for count < 2 {
		select {
		case u := <-sub.Updates():
			if u.Snapshot != nil && u.Snapshot.Phase == protocol.PhaseStopping {
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
			if u.Snapshot != nil && u.Snapshot.Phase == protocol.PhaseIdle && !u.Snapshot.Closing && !u.Snapshot.Busy {
				return
			}
		case <-ctx.Done():
			t.Fatal("child watch retained stopping after instance detached")
		}
	}
}
