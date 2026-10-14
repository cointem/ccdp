package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccdp/internal/events"
	"ccdp/internal/hooks"
	"ccdp/internal/llm"
	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
	"ccdp/internal/session"
	"ccdp/internal/supervision"
)

func TestOwnerRemainsAvailableUntilFinalizationSettles(t *testing.T) {
	a := newRuntimeAgent(t)
	a.ensureRun()
	<-a.owner.started
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	capture := finalizationCaptureFunc(func(context.Context, string, *sandbox.Sandbox) (string, error) {
		select {
		case <-a.owner.done:
			t.Error("owner exited before finalization")
		default:
		}
		close(entered)
		<-release
		return "snapshot", nil
	})
	request := supervision.FinishRequest{Capture: capture, Fact: session.ChildRunRecorded{Version: 1, Child: protocol.ChildSession{SessionID: protocol.SessionID(a.SessionID()), ParentSessionID: "parent", RootSessionID: "parent", DelegationID: "delegation", Purpose: "task", Run: protocol.RunView{ID: "run", Status: "running", WaitPolicy: "join"}}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := a.FinishRun(ctx, request); result <- err }()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	select {
	case <-entered:
	case <-deadline.C:
		t.Fatal("finalization did not start")
	}
	ready := make(chan struct{})
	select {
	case a.owner.control <- finalizeBarrier{ready: ready}:
	case <-deadline.C:
		t.Fatal("owner stopped accepting finalization messages")
	}
	select {
	case <-ready:
	case <-deadline.C:
		t.Fatal("owner did not process finalization message")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("close wait cancellation=%v", err)
	}
	select {
	case <-a.Done():
		t.Fatal("Close reported completion while finalizer still running")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	finished, err := a.FinishRun(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if finished.OutboxCursor == 0 || finished.Fact.Child.Run.ID != "run" {
		t.Fatalf("missing durable outbox: %+v", finished)
	}
	request.Output = "conflicting retry"
	if _, err := a.FinishRun(context.Background(), request); err == nil {
		t.Fatal("conflicting finalization accepted")
	}
	select {
	case <-a.owner.done:
	default:
		t.Fatal("Close returned before owner exit")
	}
}

func TestQueuedInputCallbackCanSubmitToOwner(t *testing.T) {
	testInputCallbackCanSubmitToOwner(t, "queued")
}
func TestFirstInputCallbackCanSubmitToOwner(t *testing.T) {
	testInputCallbackCanSubmitToOwner(t, "first")
}
func testInputCallbackCanSubmitToOwner(t *testing.T, callbackText string) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	provider := &childRuntimeTestProvider{name: "owner-callback", stream: func(ctx context.Context, _ llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return llm.StreamResult{}, ctx.Err()
			}
		}
		return llm.StreamResult{Text: "done", FinishReason: "stop"}, nil
	}}
	a, _ := newChildRuntimeTestAgent(t, provider)
	callbackResult := make(chan error, 1)
	dispose := a.services.evbus.Subscribe(events.TopicMessageAdded, func(_ events.Topic, payload events.Payload) {
		message, ok := payload.(events.MessageEvent)
		if !ok || message.Content != callbackText {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		receipt, err := a.Submit(ctx, protocol.Command{ID: "callback-query", SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandQuery, Query: &protocol.QueryCommand{Kind: protocol.QueryStatus}})
		if err == nil && receipt.Rejected() {
			err = fmt.Errorf("query rejected: %+v", receipt.Error)
		}
		callbackResult <- err
	})
	defer dispose()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, text := range []string{"first", "queued"} {
		cmd := protocol.NewSubmitInput(protocol.CommandID(text), protocol.SessionID(a.SessionID()), protocol.InputID(text), text, protocol.InputFollowup)
		if receipt, err := a.Submit(ctx, cmd); err != nil || receipt.Rejected() {
			t.Fatalf("submit: %+v %v", receipt, err)
		}
		if text == "first" {
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("first provider call did not start")
			}
		}
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-callbackResult:
		if err != nil {
			t.Fatalf("queued-input callback could not re-enter owner: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("queued-input notification was not delivered")
	}
}

type finalizationCaptureFunc func(context.Context, string, *sandbox.Sandbox) (string, error)

func (f finalizationCaptureFunc) Capture(ctx context.Context, workspace string, policy *sandbox.Sandbox) (string, error) {
	return f(ctx, workspace, policy)
}

func TestChildOutboxIncludesCleanupFailureAndFollowsSessionClosed(t *testing.T) {
	a := newRuntimeAgent(t)
	a.services.hooks = hooks.NewManager(hooks.Config{hooks.EventSessionEnd: []hooks.HookSpec{{Command: "echo cleanup-failed >&2; exit 2"}}}, hooks.Options{Workspace: a.services.cfg.Workspace, Sandbox: a.services.sandbox, FailClosed: true})
	request := supervision.FinishRequest{Fact: session.ChildRunRecorded{Version: 1, Child: protocol.ChildSession{SessionID: protocol.SessionID(a.SessionID()), ParentSessionID: "parent", RootSessionID: "parent", DelegationID: "delegation", Purpose: "task", Run: protocol.RunView{ID: "cleanup-run", Status: "running", WaitPolicy: "join"}}}}
	result, err := a.FinishRun(context.Background(), request)
	if err == nil || result.Fact.Child.Run.Status != "failed" || result.Fact.Child.Run.Error == "" {
		t.Fatalf("cleanup failure omitted from child outbox: result=%+v err=%v", result, err)
	}
	reader, err := session.OpenJSONLReadOnly(a.services.cfg.SessionDir, a.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	records, err := reader.Read(session.Beginning)
	if err != nil {
		t.Fatal(err)
	}
	var closed, outbox session.Cursor
	for _, record := range records {
		switch event := record.Event.(type) {
		case *session.SessionClosed:
			closed = record.Seq
			if event.Outcome != "error" {
				t.Fatalf("cleanup recorded as %s", event.Outcome)
			}
		case *session.ChildRunRecorded:
			outbox = record.Seq
		}
	}
	if closed == 0 || outbox <= closed || result.OutboxCursor != outbox {
		t.Fatalf("invalid close/outbox order: closed=%d outbox=%d returned=%d", closed, outbox, result.OutboxCursor)
	}
	result.Fact.Child.Run.Output = "mutated caller copy"
	retried, _ := a.FinishRun(context.Background(), request)
	if retried.Fact.Child.Run.Output == result.Fact.Child.Run.Output {
		t.Fatal("finalization retry shares mutable result")
	}
}

func TestCatalogCallbackCanSubmitToOwner(t *testing.T) {
	a := newRuntimeAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	var once sync.Once
	dispose := a.services.evbus.Subscribe(events.TopicToolChange, func(events.Topic, events.Payload) {
		once.Do(func() {
			receipt, err := a.Submit(ctx, protocol.Command{ID: "catalog-callback", SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandQuery, Query: &protocol.QueryCommand{Kind: protocol.QueryStatus}})
			if err == nil && receipt.Rejected() {
				err = fmt.Errorf("query rejected: %+v", receipt.Error)
			}
			result <- err
		})
	})
	defer dispose()
	receipt, err := a.Submit(ctx, protocol.Command{ID: "reload-callback", SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandReloadSettings, Reload: &protocol.ReloadCommand{}})
	if err != nil || receipt.Rejected() {
		t.Fatalf("reload: %+v %v", receipt, err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("catalog callback blocked session owner")
	}
}
