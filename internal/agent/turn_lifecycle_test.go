package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"ccdp/internal/hooks"
	"ccdp/internal/llm"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
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
	sawFinalizing := false
	deadline := time.After(5 * time.Second)
	for {
		select {
		case update, ok := <-sub.Updates():
			if !ok {
				t.Fatal("watch closed before TurnDone")
			}
			if update.State != nil {
				if update.State.Phase == protocol.PhaseFinalizing {
					sawFinalizing = true
				}
				for _, item := range update.State.Transcript {
					if item.Kind == "turn_summary" && update.State.Phase == protocol.PhaseStreaming {
						t.Fatal("completed turn still advertises streaming")
					}
				}
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
	if !sawFinalizing {
		t.Fatal("turn did not publish its finalization phase")
	}
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
			ag.services.client = client
			ag.modelsState.primaryBinding = binding
			ag.modelsState.activeModel = "cancel-model"
			// This locked state is the deterministic barrier between input
			// admission and the new goroutine's first cancellation check.
			ag.modelsState.activeBinding = binding
			ag.turn.interruptFlag = true
			ag.mu.Unlock()

			input := newTurnInput(protocol.InputID("cancel-"+tc.name), "cancel immediately", protocol.InputSteer, time.Now().UTC())
			input.historyAppended = true
			ag.startTurnInput(input)
			deadline := time.After(5 * time.Second)
			for {
				select {
				case ev := <-watchTestEvents(t, ag):
					if ev.Kind != protocol.EventTurnDone {
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

func TestTurnCommitFailurePublishesFailedTerminalOutcome(t *testing.T) {
	ag := newJournalRuntimeAgent(t, &formalCompletionProvider{})
	p := ag.persistenceHandle()
	backend := journalBackend(t, p)
	backend.mu.Lock()
	backend.store = requestJournalFailEventStore{Store: backend.store, event: session.EventTypeTurnFinished}
	backend.mu.Unlock()
	submitJournalRuntimeInput(t, ag)
	view := waitJournalRuntimeIdle(t, ag)
	if view.Busy || view.LastTurn == nil || view.LastTurn.Status != protocol.TurnFailed || view.LastTurn.Error == "" {
		t.Fatalf("TurnFinished commit failure outcome = %+v", view.LastTurn)
	}
}

func TestCancellationReachesPreparationHook(t *testing.T) {
	for _, kind := range []protocol.CommandType{protocol.CommandInterrupt, protocol.CommandStop} {
		t.Run(string(kind), func(t *testing.T) {
			provider := &formalCompletionProvider{}
			ag := formalHookAgent(t, provider, hooks.HookSpec{Command: "printf ready > hook-started; exec sleep 60"})
			events := watchTestEvents(t, ag)
			submitJournalRuntimeInput(t, ag)
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(ag.services.cfg.Workspace, "hook-started")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("preparation hook did not start")
				}
				time.Sleep(5 * time.Millisecond)
			}
			receipt, err := ag.Submit(context.Background(), protocol.Command{ID: "cancel-preparation", SessionID: protocol.SessionID(ag.SessionID()), Type: kind})
			if err != nil || receipt.Rejected() {
				t.Fatalf("cancel: %+v, %v", receipt, err)
			}
			drainUntil(t, events, 5*time.Second, func(e protocol.EventView) bool { return e.Kind == protocol.EventTurnDone })
			view, err := ag.Snapshot(context.Background())
			if err != nil || view.LastTurn == nil || view.LastTurn.Status != protocol.TurnCancelled {
				t.Fatalf("preparation cancellation: %+v, %v", view.LastTurn, err)
			}
			if got := provider.requestsCount(); got != 0 {
				t.Fatalf("cancelled hook still reached model: %d", got)
			}
		})
	}
}
