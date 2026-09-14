package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccdp/internal/llm"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/tools"
)

func waitManaged(t *testing.T, r *managedRun) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(8 * time.Second):
		t.Fatal("managed run failed to settle")
	}
}

func TestSupervisorObservationDoesNotOwnExecution(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var cancelled atomic.Bool
	p := &childRuntimeTestProvider{name: "observe"}
	p.stream = func(ctx context.Context, _ llm.CompletionRequest, delta func(string)) (llm.StreamResult, error) {
		delta("partial")
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			cancelled.Store(true)
			return llm.StreamResult{}, ctx.Err()
		}
		return llm.StreamResult{Text: "finished", FinishReason: "stop", PromptTokens: 3, CompletionTok: 2}, nil
	}
	a, _ := newChildRuntimeTestAgent(t, p)
	r, err := a.supervisor.launch(a, a.rootCtx, tools.SubagentTask{Description: "observe me"}, childPurposeTask, "call", 0)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	reader, err := a.Sessions().OpenReader(context.Background(), r.fact.Child.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := reader.Watch(context.Background(), protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	u := <-sub.Updates()
	if u.Snapshot == nil || len(u.Snapshot.Transcript) < 2 {
		t.Fatalf("missing recoverable live transcript: %+v", u.Snapshot)
	}
	_ = sub.Close()
	if cancelled.Load() {
		t.Fatal("detaching observation cancelled execution")
	}
	close(release)
	waitManaged(t, r)
	view, err := reader.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range view.Transcript {
		if item.Text == "finished" {
			found = true
		}
	}
	if !found {
		t.Fatalf("finished transcript not retained: %+v", view.Transcript)
	}
	if a.Usage().InputTokens != 3 {
		t.Fatalf("usage not accounted: %+v", a.Usage())
	}
	r.mu.Lock()
	err = a.supervisor.recordOutcome(r)
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if a.Usage().InputTokens != 3 {
		t.Fatal("replayed result counted usage twice")
	}
}

func TestSupervisorRegistersQueuedChildrenAndCancelsOnlyTarget(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	p := &childRuntimeTestProvider{name: "queue"}
	p.stream = func(ctx context.Context, _ llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return llm.StreamResult{}, ctx.Err()
		}
		return llm.StreamResult{Text: "first", FinishReason: "stop"}, nil
	}
	a, _ := newChildRuntimeTestAgent(t, p)
	a.childSlots = newChildSlots(1)
	r1, err := a.supervisor.launch(a, a.rootCtx, tools.SubagentTask{Description: "first"}, childPurposeTask, "batch", 0)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	r2, err := a.supervisor.launch(a, a.rootCtx, tools.SubagentTask{Description: "second"}, childPurposeTask, "batch", 1)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := a.Sessions().ListChildren(context.Background())
	if err != nil || len(rows) != 2 {
		t.Fatalf("queued children absent: %+v %v", rows, err)
	}
	_, err = a.Sessions().Control(context.Background(), protocol.AgentControl{ID: "cancel-queued", SessionID: r2.fact.Child.SessionID, RunID: r2.fact.Child.Run.ID, Action: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	waitManaged(t, r2)
	if !errors.Is(r2.err, context.Canceled) {
		t.Fatalf("queued cancellation: %v", r2.err)
	}
	select {
	case <-r1.done:
		t.Fatal("sibling was cancelled")
	default:
	}
	close(release)
	waitManaged(t, r1)
}

func TestSupervisorAcceptedFollowupIsJoined(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	p := &childRuntimeTestProvider{name: "followup"}
	p.stream = func(ctx context.Context, _ llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return llm.StreamResult{}, ctx.Err()
			}
			return llm.StreamResult{Text: "first answer", FinishReason: "stop"}, nil
		}
		return llm.StreamResult{Text: "followup answer", FinishReason: "stop"}, nil
	}
	a, _ := newChildRuntimeTestAgent(t, p)
	r, err := a.supervisor.launch(a, a.rootCtx, tools.SubagentTask{Description: "initial"}, childPurposeTask, "call", 0)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	_, err = a.Sessions().Control(context.Background(), protocol.AgentControl{ID: "follow", SessionID: r.fact.Child.SessionID, RunID: r.fact.Child.Run.ID, Action: "send", Text: "followup", Strategy: protocol.InputFollowup})
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	waitManaged(t, r)
	if r.err != nil || r.fact.Child.Run.Output != "followup answer" || calls.Load() != 2 {
		t.Fatalf("accepted followup lost: %+v %v calls=%d", r.fact.Child.Run, r.err, calls.Load())
	}
	_, err = a.Sessions().Control(context.Background(), protocol.AgentControl{ID: "late", SessionID: r.fact.Child.SessionID, Action: "send", Text: "late"})
	if err == nil {
		t.Fatal("settled run accepted input")
	}
}

func TestSupervisorContinueKeepsOriginalOutcomeAndRejectsStaleControl(t *testing.T) {
	p := &childRuntimeTestProvider{name: "continue"}
	p.stream = func(_ context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		text := "answer"
		for _, m := range req.Messages {
			if m.Role == "user" {
				text, _ = m.Content.(string)
			}
		}
		return llm.StreamResult{Text: text, FinishReason: "stop", PromptTokens: 2, CompletionTok: 1}, nil
	}
	a, cfg := newChildRuntimeTestAgent(t, p)
	r, err := a.supervisor.launch(a, a.rootCtx, tools.SubagentTask{Description: "original"}, childPurposeTask, "call", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitManaged(t, r)
	old := r.fact.Child.Run
	next, err := a.Sessions().Control(context.Background(), protocol.AgentControl{ID: "continue-command", SessionID: r.fact.Child.SessionID, RunID: old.ID, Action: "continue", Text: "new input"})
	if err != nil {
		t.Fatal(err)
	}
	nextRun, err := a.supervisor.lookup(r.fact.Child.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	waitManaged(t, nextRun)
	if nextRun.err != nil {
		t.Fatal(nextRun.err)
	}
	if next.ID == old.ID || r.fact.Child.Run.Output != "original" || nextRun.fact.Child.Run.Output != "new input" {
		t.Fatalf("run identities/outcomes changed: old=%+v next=%+v", old, nextRun.fact.Child.Run)
	}
	store, err := session.OpenJSONLReadOnly(cfg.SessionDir, string(r.fact.Child.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	records, err := store.Read(0)
	if err != nil {
		t.Fatal(err)
	}
	ready := 0
	for _, record := range records {
		if f, ok := record.Event.(*session.ChildRunRecorded); ok && f.DeliveryID != "" {
			ready++
		}
	}
	if ready != 2 {
		t.Fatalf("durable child outbox has %d results", ready)
	}
	_, err = a.Sessions().Control(context.Background(), protocol.AgentControl{ID: "stale", SessionID: r.fact.Child.SessionID, RunID: old.ID, Action: "cancel"})
	if err == nil {
		t.Fatal("stale control accepted")
	}
}

func TestTranscriptSnapshotIsBoundedAndIndependent(t *testing.T) {
	tx := &transcriptState{}
	for i := 0; i < transcriptWindow+5; i++ {
		tx.mu.Lock()
		tx.put(protocol.TranscriptItem{ID: nextRuntimeID("item"), Kind: "tool", Text: strings.Repeat("x", transcriptTextLimit+100), Args: []byte(`{"a":1}`)})
		tx.mu.Unlock()
	}
	items, more := tx.snapshot()
	if !more || len(items) != transcriptWindow || !items[0].Truncated {
		t.Fatal("transcript is not bounded")
	}
	items[0].Args[0] = 'x'
	again, _ := tx.snapshot()
	if again[0].Args[0] == 'x' {
		t.Fatal("snapshot shares mutable arguments")
	}
}
