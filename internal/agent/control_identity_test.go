package agent

import (
	"context"
	"testing"

	"ccdp/internal/protocol"
)

func TestControlRetryAndLateDeliveryKeepOriginalTurn(t *testing.T) {
	a := acceptanceAgent(t, true)
	old, cancelOld := context.WithCancel(context.Background())
	defer cancelOld()
	a.mu.Lock()
	a.sequences.turnSeq = 1
	a.turn.turnCancel = cancelOld
	a.mu.Unlock()
	cmd := protocol.Command{ID: "old-stop", SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandStop}
	target, signal := a.captureCommandCancellation(cmd)
	if signal == nil {
		t.Fatal("missing initial cancellation")
	}
	signal()
	if old.Err() == nil {
		t.Fatal("old turn was not cancelled")
	}
	next, cancelNext := context.WithCancel(context.Background())
	defer cancelNext()
	a.mu.Lock()
	a.sequences.turnSeq = 2
	a.turn.turnCancel = cancelNext
	a.mu.Unlock()
	retryTarget, retrySignal := a.captureCommandCancellation(cmd)
	if retrySignal != nil || *retryTarget != *target {
		t.Fatal("retry acquired the next turn")
	}
	// Exercise the owner dispatch path after the run has advanced. There is no
	// running owner yet, so this is an exclusive deterministic handoff fixture.
	receipt := a.dispatchCommandForTurn(cmd, true, target)
	if receipt.Rejected() {
		t.Fatalf("late cancellation failed: %+v", receipt)
	}
	if next.Err() != nil {
		t.Fatal("late delivery cancelled the next turn")
	}
	receipt, err := a.Submit(context.Background(), cmd)
	if err != nil || receipt.Rejected() {
		t.Fatalf("retry failed: %+v %v", receipt, err)
	}
	if next.Err() != nil {
		t.Fatal("public retry cancelled the next turn")
	}
}

func TestRejectedControlDoesNotCancelExecution(t *testing.T) {
	a := acceptanceAgent(t, true)
	running, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.mu.Lock()
	a.turn.turnCancel = cancel
	a.mu.Unlock()
	cmd := protocol.Command{ID: "stale-stop", SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandStop, ExpectedRevision: ^uint64(0)}
	receipt, err := a.Submit(context.Background(), cmd)
	if err != nil || !receipt.Rejected() {
		t.Fatalf("expected stale receipt: %+v %v", receipt, err)
	}
	if running.Err() != nil {
		t.Fatal("stale command cancelled execution")
	}
	ctx, cancelSubmit := context.WithCancel(context.Background())
	cancelSubmit()
	cmd.ID, cmd.ExpectedRevision = "cancelled-stop", 0
	if _, err := a.Submit(ctx, cmd); err != context.Canceled {
		t.Fatalf("expected cancelled submit: %v", err)
	}
	if running.Err() != nil {
		t.Fatal("unsubmitted command cancelled execution")
	}
}
