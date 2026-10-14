package agent

import (
	"context"
	"testing"
	"time"

	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/supervision"
)

type pausedChildSubmit struct {
	supervision.ChildRuntime
	entered chan struct{}
	release chan struct{}
}

func (p *pausedChildSubmit) Submit(ctx context.Context, cmd protocol.Command) (protocol.Receipt, error) {
	close(p.entered)
	select {
	case <-p.release:
	case <-ctx.Done():
		return protocol.Receipt{}, ctx.Err()
	}
	return p.ChildRuntime.Submit(ctx, cmd)
}

func TestChildCommandDoesNotHoldRunLockOrAllowEarlySettlement(t *testing.T) {
	a := newRuntimeAgent(t)
	a.mu.Lock()
	a.lifecycle.lastTurn = &protocol.TurnOutcome{TurnID: "completed-turn", Status: protocol.TurnSucceeded}
	a.mu.Unlock()
	child := &pausedChildSubmit{ChildRuntime: a, entered: make(chan struct{}), release: make(chan struct{})}
	id := protocol.SessionID(a.SessionID())
	run := &managedRun{agent: child, parent: a, fact: session.ChildRunRecorded{Child: protocol.ChildSession{SessionID: id, Purpose: childPurposeTask, Run: protocol.RunView{ID: "run", Status: "running"}}}}
	supervisor := testSupervisor(a, run)
	boundary := childRunBoundary{supervisor: supervisor, run: run, child: child}
	changed := boundary.Changes()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := supervisor.Control(ctx, protocol.AgentControl{ID: "interrupt", SessionID: id, RunID: "run", Action: "interrupt"})
		result <- err
	}()
	select {
	case <-child.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	checked := make(chan bool, 1)
	go func() { _, done, _ := boundary.FinishIfIdle(ctx, ""); checked <- done }()
	select {
	case done := <-checked:
		if done {
			t.Error("settled while an admitted command was in flight")
		}
	case <-ctx.Done():
		t.Error("cross-owner Submit held the run lock")
	}
	close(child.release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-ctx.Done():
		t.Fatal("command completion did not wake idle check")
	}
	_, done, err := boundary.FinishIfIdle(ctx, "")
	if err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
}

func TestMailDuringContinuationAdmissionReachesNewRun(t *testing.T) {
	a := newRuntimeAgent(t)
	id := protocol.SessionID("mail-child")
	row := protocol.ChildSession{SessionID: id, ParentSessionID: protocol.SessionID(a.SessionID()), RootSessionID: protocol.SessionID(a.SessionID()), DelegationID: "delegation", Purpose: childPurposeTask, Run: protocol.RunView{ID: "old", Status: "succeeded", WaitPolicy: "notify"}}
	previous := &managedRun{parent: a, fact: session.ChildRunRecorded{Version: 1, Child: row}}
	s := a.services.supervisor
	s.directory.Restore(row, previous, true)
	row.Run = protocol.RunView{ID: "new", Status: "queued", WaitPolicy: "notify"}
	next := &managedRun{parent: a, fact: session.ChildRunRecorded{Version: 1, Child: row}}
	if err := s.reserveRun(next, previous); err != nil {
		t.Fatal(err)
	}
	defer func() { s.directory.Settled(next); s.directory.WorkerDone() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- a.sendAgentMessage(ctx, "handoff-mail", string(id), "keep this message") }()
	// A sender must not acknowledge mail to the old run while admission is pending.
	select {
	case err := <-result:
		t.Fatalf("mail bypassed handoff: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	s.finishRunAdmission(next, nil)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	previous.mu.Lock()
	oldCount := len(previous.fact.PendingMessages)
	previous.mu.Unlock()
	next.mu.Lock()
	newCount := len(next.fact.PendingMessages)
	next.mu.Unlock()
	if oldCount != 0 || newCount != 1 {
		t.Fatalf("mail lost at handoff: old=%d new=%d", oldCount, newCount)
	}
	records, err := a.persistenceHandle().Read(session.Beginning)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, record := range records {
		if fact, ok := record.Event.(*session.ChildRunRecorded); ok && fact.Child.Run.ID == "new" && len(fact.PendingMessages) == 1 {
			found = true
		}
	}
	if !found {
		t.Fatal("new run mailbox was acknowledged without a durable record")
	}
}
