package agent

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
)

func queuePreparationInputs(t *testing.T, a *Agent, inputs ...protocol.InputStrategy) {
	t.Helper()
	a.mu.Lock()
	a.turn.busy = true
	a.mu.Unlock()
	defer func() { a.mu.Lock(); a.turn.busy = false; a.mu.Unlock() }()
	for i, strategy := range inputs {
		id := protocol.InputID(string(rune('a' + i)))
		receipt := a.applySubmitInput(protocol.NewSubmitInput(protocol.CommandID(id), "", id, string(id), strategy))
		if receipt.Rejected() {
			t.Fatal(receipt.Error)
		}
	}
}

func TestStepPreparationClaimsOneSteerAcrossRepeatedIntake(t *testing.T) {
	provider := &childRuntimeTestProvider{name: "prepare-input", stream: func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
		return llm.StreamResult{}, nil
	}}
	a := newJournalRuntimeAgent(t, provider)
	queuePreparationInputs(t, a, protocol.InputMessage, protocol.InputSteer, protocol.InputSteer, protocol.InputMessage)
	state := stepPreparation{}
	if err := a.consumeStepInputs(&state); err != nil {
		t.Fatal(err)
	}
	if err := a.consumeStepInputs(&state); err != nil {
		t.Fatal(err)
	}
	if !state.steerClaimed {
		t.Fatal("steer was not claimed")
	}
	a.mu.Lock()
	pending := a.conversation.durable.PendingInputViews()
	a.mu.Unlock()
	if len(pending) != 1 || pending[0].ID != "c" {
		t.Fatalf("pending=%+v", pending)
	}
	history := a.History()
	if len(history) != 3 || history[0].Content != "a" || history[1].Content != "d" || history[2].Content != "b" {
		t.Fatalf("history=%+v", history)
	}
}

func TestStepPreparationDoesNotSkipFollowupToClaimSteer(t *testing.T) {
	provider := &childRuntimeTestProvider{name: "prepare-order", stream: func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
		return llm.StreamResult{}, nil
	}}
	a := newJournalRuntimeAgent(t, provider)
	queuePreparationInputs(t, a, protocol.InputFollowup, protocol.InputSteer, protocol.InputMessage)
	state := stepPreparation{}
	if err := a.consumeStepInputs(&state); err != nil {
		t.Fatal(err)
	}
	if state.steerClaimed {
		t.Fatal("steer overtook followup")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	reason, err := a.waitAgentEvent(ctx)
	if err != nil || reason != "no_pending_work" {
		t.Fatalf("wait=%s %v", reason, err)
	}
	a.mu.Lock()
	pending := a.conversation.durable.PendingInputViews()
	a.mu.Unlock()
	if len(pending) != 2 || pending[0].ID != "a" || pending[1].ID != "b" {
		t.Fatalf("pending=%+v", pending)
	}
}

func TestParentWaitingForChildReleasesStepSnapshot(t *testing.T) {
	childStarted := make(chan struct{})
	finishChild := make(chan struct{})
	provider := &childRuntimeTestProvider{name: "step-wait"}
	provider.stream = func(ctx context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		for _, msg := range req.Messages {
			if msg.Role == "system" {
				if text, ok := msg.Content.(string); ok && strings.Contains(text, "You are a ccdp sub-agent") {
					select {
					case <-childStarted:
					default:
						close(childStarted)
					}
					select {
					case <-finishChild:
						return llm.StreamResult{Text: "child done", FinishReason: "stop"}, nil
					case <-ctx.Done():
						return llm.StreamResult{}, ctx.Err()
					}
				}
			}
		}
		for _, msg := range req.Messages {
			if msg.Role == "tool" {
				return llm.StreamResult{Text: "waiting for child", FinishReason: "stop"}, nil
			}
		}
		return llm.StreamResult{ToolCalls: []llm.ToolCall{{ID: "spawn", Type: "function", Function: llm.Function{Name: "SpawnAgent", Arguments: llm.ArgumentsJSON(`{"task":"inspect","role":"explorer"}`)}}}, FinishReason: "tool_calls"}, nil
	}
	a := newJournalRuntimeAgent(t, provider)
	submitChildRuntimeTestInput(t, a, "wait-release", "delegate inspection")
	defer close(finishChild)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		view, err := a.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if view.Phase == protocol.PhaseWaitingAgents {
			a.mu.Lock()
			snapshot := a.children.childStep
			a.mu.Unlock()
			if snapshot != nil {
				t.Fatal("waiting parent retained frozen step")
			}
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("parent did not wait: %+v", view.LastTurn)
		}
	}
}

func TestStepPreparationClaimsSteerArrivingDuringCompaction(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	finalRequest := make(chan llm.CompletionRequest, 1)
	provider := &childRuntimeTestProvider{name: "prepare-after-compact"}
	provider.stream = func(ctx context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		if len(req.Messages) > 0 && req.Messages[0].Content == compactSystem {
			close(entered)
			select {
			case <-release:
				return llm.StreamResult{Text: "earlier work summarized", FinishReason: "stop"}, nil
			case <-ctx.Done():
				return llm.StreamResult{}, ctx.Err()
			}
		}
		finalRequest <- req
		return llm.StreamResult{Text: "done", FinishReason: "stop"}, nil
	}
	a := newJournalRuntimeAgent(t, provider)
	a.services.cfg.CompactThreshold = .01
	for i := 0; i < 12; i++ {
		if err := a.appendHistory(messages.Message{Role: messages.RoleUser, Content: strings.Repeat("earlier work ", 100)}); err != nil {
			t.Fatal(err)
		}
	}
	submitChildRuntimeTestInput(t, a, "compact-entry", "continue work")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("compaction did not start")
	}
	submitChildRuntimeTestInput(t, a, "compact-steer", "change requested during compaction")
	close(release)
	select {
	case req := <-finalRequest:
		found := false
		for _, msg := range req.Messages {
			if msg.Content == "change requested during compaction" {
				found = true
			}
		}
		if !found {
			t.Fatal("post-compaction request missed queued steer")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request did not follow compaction")
	}
	waitChildRuntimeTestIdle(t, a)
}

func TestContextOverflowRecoveryIsBounded(t *testing.T) {
	var mainCalls, summaryCalls atomic.Int32
	provider := &childRuntimeTestProvider{name: "bounded-context-recovery"}
	provider.stream = func(_ context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		if len(req.Messages) > 0 && req.Messages[0].Content == compactSystem {
			summaryCalls.Add(1)
			return llm.StreamResult{Text: "old state summarized", FinishReason: "stop"}, nil
		}
		mainCalls.Add(1)
		return llm.StreamResult{}, fmt.Errorf("remote request: %w", llm.ErrContextWindowExceeded)
	}
	a := newJournalRuntimeAgent(t, provider)
	for i := 0; i < 12; i++ {
		if err := a.appendHistory(messages.Message{Role: messages.RoleAssistant, Content: strings.Repeat("earlier work details ", 100)}); err != nil {
			t.Fatal(err)
		}
	}
	submitChildRuntimeTestInput(t, a, "overflow", "continue work")
	view := waitChildRuntimeTestIdle(t, a)
	if mainCalls.Load() != 2 || summaryCalls.Load() != 1 || view.LastTurn.Status != protocol.TurnFailed {
		t.Fatalf("calls=%d summaries=%d outcome=%+v", mainCalls.Load(), summaryCalls.Load(), view.LastTurn)
	}
}

func TestInterruptDuringContextRecoveryCancelsTurn(t *testing.T) {
	entered := make(chan struct{})
	provider := &childRuntimeTestProvider{name: "cancel-context-recovery"}
	provider.stream = func(ctx context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		if len(req.Messages) > 0 && req.Messages[0].Content == compactSystem {
			close(entered)
			<-ctx.Done()
			return llm.StreamResult{}, ctx.Err()
		}
		return llm.StreamResult{}, llm.ErrContextWindowExceeded
	}
	a := newJournalRuntimeAgent(t, provider)
	for i := 0; i < 12; i++ {
		if err := a.appendHistory(messages.Message{Role: messages.RoleAssistant, Content: strings.Repeat("earlier work details ", 100)}); err != nil {
			t.Fatal(err)
		}
	}
	submitChildRuntimeTestInput(t, a, "cancel-overflow", "continue work")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery did not start")
	}
	a.interrupt()
	view := waitChildRuntimeTestIdle(t, a)
	if view.LastTurn.Status != protocol.TurnCancelled {
		t.Fatalf("cancellation became failure: %+v", view.LastTurn)
	}
}
