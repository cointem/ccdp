package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
)

func TestM1WatchInitialSnapshotIsAtomicWithStateUpdate(t *testing.T) {
	ag := newRuntimeAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub, err := ag.Watch(ctx, protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	first, ok := <-sub.Updates()
	if !ok || first.Type != protocol.UpdateSnapshot || first.Snapshot == nil {
		t.Fatalf("first watch update = %#v, ok=%v", first, ok)
	}
	ag.setPhase(protocol.PhaseStreaming)
	select {
	case update, ok := <-sub.Updates():
		if !ok || update.Snapshot == nil || update.Snapshot.Phase != protocol.PhaseStreaming {
			t.Fatalf("state update after initial snapshot = %#v, ok=%v", update, ok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("state update was lost after watch registration")
	}
}

func TestM1WatchRegistrationAndPublishInterleave(t *testing.T) {
	ag := newRuntimeAgent(t)
	defer ag.Close()

	// Hold the publication lock while Watch and a state publisher rendezvous.
	// Both operations are then released together, forcing the implementation to
	// make registration and the initial snapshot one atomic critical section.
	ag.watchMu.Lock()
	released := false
	defer func() {
		if !released {
			ag.watchMu.Unlock()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type watchResult struct {
		sub protocol.Subscription
		err error
	}
	started := make(chan struct{})
	result := make(chan watchResult, 1)
	go func() {
		close(started)
		sub, err := ag.Watch(ctx, protocol.Cursor{})
		result <- watchResult{sub: sub, err: err}
	}()
	<-started

	stateDone := make(chan struct{})
	go func() {
		ag.setPhase(protocol.PhaseStreaming)
		close(stateDone)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		ag.mu.Lock()
		phase := ag.phase
		ag.mu.Unlock()
		if phase == protocol.PhaseStreaming {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("state publisher did not reach the phase update barrier")
		}
		time.Sleep(time.Millisecond)
	}

	ag.watchMu.Unlock()
	released = true
	select {
	case <-stateDone:
	case <-time.After(2 * time.Second):
		t.Fatal("state publisher did not complete")
	}
	var wr watchResult
	select {
	case wr = <-result:
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not complete after publication lock release")
	}
	if wr.err != nil {
		t.Fatal(wr.err)
	}
	defer wr.sub.Close()
	first, ok := <-wr.sub.Updates()
	if !ok || first.Type != protocol.UpdateSnapshot || first.Snapshot == nil {
		t.Fatalf("interleaved initial update = %#v, ok=%v", first, ok)
	}
	if first.Snapshot.Phase != protocol.PhaseStreaming {
		select {
		case update, ok := <-wr.sub.Updates():
			if !ok || update.Snapshot == nil || update.Snapshot.Phase != protocol.PhaseStreaming {
				t.Fatalf("interleaved state update = %#v, ok=%v", update, ok)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("interleaved state update was lost")
		}
	}
}

func TestM1WatchSaturationRetainsTerminalResync(t *testing.T) {
	ag := newRuntimeAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub, err := ag.Watch(ctx, protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	// Do not consume the initial snapshot. The runtime reserves one slot for
	// resync, so a slow reader must observe the marker instead of silent loss.
	for i := 0; i < 256; i++ {
		ag.emitStatus("update-%d", i)
	}
	seenResync := false
	deadline := time.After(2 * time.Second)
	for {
		select {
		case update, ok := <-sub.Updates():
			if !ok {
				if !seenResync {
					t.Fatal("watch closed without a resync marker")
				}
				return
			}
			if update.Type == protocol.UpdateResyncRequired {
				seenResync = true
			}
		case <-deadline:
			t.Fatalf("slow watch did not receive terminal resync; seen=%v", seenResync)
		}
	}
}

func TestM1WatchResyncSnapshotRetainsFailedTurn(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startOnce.Do(func() { close(started) })
		<-release
		http.Error(w, `{"error":{"message":"provider failed"}}`, http.StatusBadGateway)
	}))
	defer server.Close()

	dir := t.TempDir()
	cfg := config.Default()
	cfg.Workspace = dir
	cfg.SessionDir = dir + "/sessions"
	cfg.BaseURL = server.URL
	cfg.APIKey = "test-key"
	cfg.PermissionMode = string(permissions.ModeBypass)
	ag, err := New(&cfg, make(chan Event, 1024))
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()

	sub, err := ag.Watch(context.Background(), protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}

	cmd := protocol.NewSubmitInput("failed-command", protocol.SessionID(ag.SessionID()), "failed-input", "fail", protocol.InputSteer)
	if receipt, err := ag.Submit(context.Background(), cmd); err != nil || receipt.Rejected() {
		t.Fatalf("submit: receipt=%+v err=%v", receipt, err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("provider request did not start")
	}

	// Leave the subscription unread while enough transient events arrive to
	// force its bounded queue into the explicit terminal resync state.
	for i := 0; i < 256; i++ {
		ag.emitStatus("slow-watch-%d", i)
	}
	seenResync := false
	closed := false
	deadline := time.After(3 * time.Second)
	for !closed {
		select {
		case update, ok := <-sub.Updates():
			if !ok {
				closed = true
				continue
			}
			if update.Type == protocol.UpdateResyncRequired {
				seenResync = true
			}
		case <-deadline:
			t.Fatalf("watch did not close after resync, seen=%v", seenResync)
		}
	}
	if !seenResync {
		t.Fatal("watch closed without a resync marker")
	}

	close(release)
	var failed protocol.SessionView
	deadline = time.After(5 * time.Second)
	for {
		failed, err = ag.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if failed.LastTurn != nil && failed.LastTurn.Status == protocol.TurnFailed {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("failed turn outcome not retained: %+v", failed.LastTurn)
		case <-time.After(5 * time.Millisecond):
		}
	}
	if failed.LastTurn.TurnID == "" || !strings.Contains(failed.LastTurn.Error, "LLM error") {
		t.Fatalf("failed turn outcome = %+v", failed.LastTurn)
	}

	// A fresh Watch starts from the recovered snapshot and can report the
	// provider failure even though the original stream lost EventError/Done.
	reconnected, err := ag.Watch(context.Background(), protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	defer reconnected.Close()
	select {
	case update, ok := <-reconnected.Updates():
		if !ok || update.Type != protocol.UpdateSnapshot || update.Snapshot == nil {
			t.Fatalf("reconnected first update = %#v, ok=%v", update, ok)
		}
		if update.Snapshot.LastTurn == nil || update.Snapshot.LastTurn.Status != protocol.TurnFailed || update.Snapshot.LastTurn.Error == "" {
			t.Fatalf("reconnected snapshot lost failed outcome: %+v", update.Snapshot.LastTurn)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reconnected Watch did not deliver initial snapshot")
	}
}

func TestM1WatchCloseChannelAndConcurrentCloseAreIdempotent(t *testing.T) {
	ag := newRuntimeAgent(t)
	ctx := context.Background()
	sub, err := ag.Watch(ctx, protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sub.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()
	}
	wg.Wait()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-sub.Updates():
			if !ok {
				return
			}
			// A buffered initial snapshot may be observed after Close; drain it
			// before asserting the channel's terminal closure.
		case <-deadline:
			t.Fatal("subscription channel did not close")
		}
	}
}

func TestM1CloseConcurrentWithoutEventConsumerJoins(t *testing.T) {
	ag := newRuntimeAgent(t)
	// Fill the internal event queue and leave the external event channel
	// unconsumed. Close must cancel the dispatcher rather than waiting forever
	// for a UI reader.
	for i := 0; i < 1024; i++ {
		ag.emitStatus("queued")
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := ag.CloseContext(context.Background()); err != nil {
				t.Errorf("CloseContext: %v", err)
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("concurrent Close did not join")
	}
	select {
	case <-ag.Done():
	case <-time.After(1 * time.Second):
		t.Fatal("Done did not close")
	}
}

func TestM1CommandAndInputIDConflictsReject(t *testing.T) {
	ag := newRuntimeAgent(t)
	session := protocol.SessionID(ag.SessionID())
	first := protocol.NewSetModel("same-command", session, "one")
	if receipt := ag.applyCommand(first); receipt.Rejected() {
		t.Fatalf("first command rejected: %+v", receipt)
	}
	conflict := protocol.NewSetModel("same-command", session, "two")
	receipt := ag.applyCommand(conflict)
	if !receipt.Rejected() || receipt.Error == nil || receipt.Error.Code != protocol.ErrorInvalidCommand {
		t.Fatalf("conflicting command id = %+v", receipt)
	}

	ag.mu.Lock()
	ag.busy = true
	ag.mu.Unlock()
	input1 := protocol.NewSubmitInput("input-command-1", session, "same-input", "first", protocol.InputSteer)
	if receipt := ag.applyCommand(input1); receipt.Rejected() {
		t.Fatalf("first input rejected: %+v", receipt)
	}
	input2 := protocol.NewSubmitInput("input-command-2", session, "same-input", "second", protocol.InputSteer)
	receipt = ag.applyCommand(input2)
	if !receipt.Rejected() || receipt.Error == nil || receipt.Error.Code != protocol.ErrorInvalidCommand {
		t.Fatalf("conflicting input id = %+v", receipt)
	}
}
