package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"ccdp/internal/llm"
	"ccdp/internal/protocol"
)

// TestTurnDoneIsAnIdleBoundary uses the typed terminal update itself as the
// synchronization barrier. At that point the snapshot must already contain
// the terminal outcome, and commands submitted immediately afterward must not
// be rejected merely because an internal terminal publisher is still settling.
func TestTurnDoneIsAnIdleBoundary(t *testing.T) {
	f := &fakeLLM{script: []string{"text:done"}}
	ag, _ := newTestAgent(t, f)
	go ag.Run()

	sub, err := ag.Watch(context.Background(), protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if first, ok := <-sub.Updates(); !ok || first.Type != protocol.UpdateSnapshot {
		t.Fatalf("initial watch update = %#v, ok=%v", first, ok)
	}

	cmd := protocol.NewSubmitInput("lifecycle-submit", protocol.SessionID(ag.SessionID()), "lifecycle-input", "finish", protocol.InputSteer)
	if receipt, err := ag.Submit(context.Background(), cmd); err != nil || receipt.Rejected() {
		t.Fatalf("submit: receipt=%+v err=%v", receipt, err)
	}

	var done protocol.EventView
	deadline := time.After(5 * time.Second)
	for {
		select {
		case update, ok := <-sub.Updates():
			if !ok {
				t.Fatal("watch closed before TurnDone")
			}
			if update.Type == protocol.UpdateStream && update.Event != nil && update.Event.Kind == protocol.EventTurnDone {
				done = *update.Event
				goto terminal
			}
		case <-deadline:
			t.Fatal("typed Watch did not deliver TurnDone")
		}
	}

terminal:
	if done.TurnID == "" {
		t.Fatalf("TurnDone has no turn identity: %+v", done)
	}
	view, err := ag.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.Busy || view.LastTurn == nil || view.LastTurn.TurnID != done.TurnID || view.LastTurn.Status != protocol.TurnSucceeded {
		t.Fatalf("snapshot at TurnDone boundary = %+v, event=%+v", view, done)
	}
	if len(view.Transcript) == 0 || view.Transcript[len(view.Transcript)-1].Kind != "turn_summary" || view.Transcript[len(view.Transcript)-1].DurationMs <= 0 {
		t.Fatalf("completed turn omitted its durable elapsed time: %+v", view.Transcript)
	}

	// Exercise both an ordinary safety command and the asynchronous fork path
	// at the exact terminal boundary. Neither may report the old turn as busy.
	sandboxCmd := protocol.Command{ID: "lifecycle-sandbox", SessionID: protocol.SessionID(ag.SessionID()), Type: protocol.CommandSetSandboxPolicy,
		SandboxPolicy: &protocol.SetSandboxPolicy{Policy: protocol.SandboxPolicy{NetworkAccess: true}}}
	if receipt, err := ag.Submit(context.Background(), sandboxCmd); err != nil || receipt.Rejected() {
		t.Fatalf("sandbox immediately after TurnDone = %+v", receipt)
	}
	forkCmd := protocol.Command{ID: "lifecycle-fork", SessionID: protocol.SessionID(ag.SessionID()), Type: protocol.CommandFork,
		Fork: &protocol.Fork{Count: -1}}
	if receipt, err := ag.Submit(context.Background(), forkCmd); err != nil || receipt.Rejected() {
		t.Fatalf("fork immediately after TurnDone = %+v", receipt)
	}
}

func TestTurnAdmissionCancellationIsNotClearedByRunTurn(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop bool
	}{
		{name: "interrupt", stop: false},
		{name: "stop", stop: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				writeSSE(w, `{"choices":[{"delta":{"content":"must not run"},"finish_reason":"stop"}]}`)
				writeSSE(w, `[DONE]`)
			}))
			defer server.Close()

			ag := newRuntimeAgent(t)
			client, err := llm.NewClient(llm.Config{BaseURL: server.URL, APIKey: "cancel-key", Model: "cancel-model", Timeout: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			ag.mu.Lock()
			binding := modelBinding{model: "cancel-model", provider: client.Name(), endpoint: server.URL, client: client, version: 1}
			ag.client = client
			ag.primaryBinding = binding
			ag.activeModel = "cancel-model"
			// This locked state is the deterministic barrier between input
			// admission and the new goroutine's first cancellation check.
			ag.activeBinding = binding
			ag.interruptFlag = !tc.stop
			ag.stop = tc.stop
			ag.mu.Unlock()

			input := newTurnInput(protocol.InputID("cancel-"+tc.name), "cancel immediately", protocol.InputSteer, time.Now().UTC())
			input.historyAppended = true
			ag.startTurnInput(input)
			deadline := time.After(5 * time.Second)
			for {
				select {
				case ev := <-ag.events:
					if ev.Type != EventTurnDone {
						continue
					}
					if got := requests.Load(); got != 0 {
						t.Fatalf("provider ran after %s admission cancellation: %d requests", tc.name, got)
					}
					view, snapshotErr := ag.Snapshot(context.Background())
					if snapshotErr != nil {
						t.Fatal(snapshotErr)
					}
					if view.LastTurn == nil || view.LastTurn.Status != protocol.TurnCancelled {
						t.Fatalf("%s cancellation outcome = %+v", tc.name, view.LastTurn)
					}
					return
				case <-deadline:
					t.Fatalf("%s cancelled turn did not complete", tc.name)
				}
			}
		})
	}
}

func TestSaveFailurePublishesFailedTerminalOutcome(t *testing.T) {
	ag := newRuntimeAgent(t)
	if err := ag.closePersistence(); err != nil {
		t.Fatalf("close persistence: %v", err)
	}
	ag.mu.Lock()
	ag.busy = true
	ag.turnSeq = 1
	ag.lastTurn = &protocol.TurnOutcome{TurnID: "1", Status: protocol.TurnRunning}
	ag.mu.Unlock()

	// Closing persistence makes the terminal Save fail deterministically. The
	// terminal projection must remain failed; TurnDone must not turn it into a
	// success merely because the turn body returned.
	ag.turnFinished()
	view, err := ag.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.Busy || view.LastTurn == nil || view.LastTurn.Status != protocol.TurnFailed || view.LastTurn.Error == "" {
		t.Fatalf("Save failure terminal outcome = %+v", view)
	}
}
