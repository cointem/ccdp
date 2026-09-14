package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/llm"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/tools"
)

func TestSupervisorInterruptCancelsQueuedInputsAndPreservesReceipt(t *testing.T) {
	started := make(chan struct{})
	p := &childRuntimeTestProvider{name: "interrupt-pending", stream: func(ctx context.Context, _ llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		close(started)
		<-ctx.Done()
		return llm.StreamResult{}, ctx.Err()
	}}
	a, _ := newChildRuntimeTestAgent(t, p)
	r, err := a.supervisor.launch(a, a.rootCtx, tools.SubagentTask{Description: "block"}, childPurposeTask, "call", 0)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	r.mu.Lock()
	row := r.fact.Child
	r.mu.Unlock()
	reader, err := a.Sessions().OpenReader(context.Background(), row.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	cmd := protocol.NewSubmitInput("reader-command", row.SessionID, "different-input-id", "queued followup", protocol.InputFollowup)
	cmd.ExpectedRunID = row.Run.ID
	receipt, err := reader.Submit(context.Background(), cmd)
	if err != nil || receipt.Status != protocol.ReceiptScheduled {
		t.Fatalf("lost scheduled receipt: %+v %v", receipt, err)
	}
	_, err = reader.Submit(context.Background(), protocol.Command{ID: "interrupt", SessionID: row.SessionID, ExpectedRunID: row.Run.ID, Type: protocol.CommandInterrupt})
	if err != nil {
		t.Fatal(err)
	}
	waitManaged(t, r)
	if r.fact.Child.Run.Status != "cancelled" {
		t.Fatalf("outcome: %+v", r.fact.Child.Run)
	}
	records, _, err := a.supervisor.readSessionRecords(context.Background(), row.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var cancelled bool
	for _, record := range records {
		if e, ok := record.Event.(*session.InputCancelled); ok && e.InputID == "different-input-id" {
			cancelled = true
		}
	}
	if !cancelled {
		t.Fatal("queued input cancellation not persisted")
	}
}

func TestSupervisorMemoryContinueRetainsHistoryAndCommandIdentity(t *testing.T) {
	p := &childRuntimeTestProvider{name: "memory", stream: func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
		return llm.StreamResult{Text: "memory answer", FinishReason: "stop"}, nil
	}}
	cfg := config.Default()
	cfg.Workspace = t.TempDir()
	cfg.SessionDir = filepath.Join(cfg.Workspace, "never-created")
	cfg.NoSessionPersistence = true
	cfg.Model = "memory-model"
	cfg.PermissionMode = "bypassPermissions"
	registry := plugin.NewModelRegistry()
	registry.Register(p)
	registry.Route(cfg.Model, p.Name())
	a, err := NewWithOptions(&cfg, nil, Options{EffectiveConfigFrozen: true, ProviderRegistry: registry})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	r, err := a.supervisor.launch(a, a.rootCtx, tools.SubagentTask{Description: "memory original"}, childPurposeTask, "call", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitManaged(t, r)
	row := r.fact.Child
	cmd := protocol.AgentControl{ID: "memory-continue", SessionID: row.SessionID, RunID: row.Run.ID, Action: "continue", Text: "memory next"}
	next, err := a.Sessions().Control(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	nr, _ := a.supervisor.lookup(row.SessionID)
	waitManaged(t, nr)
	if nr.err != nil {
		t.Fatal(nr.err)
	}
	again, err := a.Sessions().Control(context.Background(), cmd)
	if err != nil || next.ID != again.ID {
		t.Fatalf("retry: %+v %v", again, err)
	}
	cmd.Text = "different intent"
	if _, err = a.Sessions().Control(context.Background(), cmd); err == nil {
		t.Fatal("conflicting command reused")
	}
	page, err := a.Sessions().ReadTranscript(context.Background(), row.SessionID, 0, 64)
	if err != nil {
		t.Fatal(err)
	}
	var original, nextInput bool
	for _, item := range page.Items {
		original = original || item.Text == "memory original"
		nextInput = nextInput || item.Text == "memory next"
	}
	if !original || !nextInput {
		t.Fatalf("memory history lost: %+v", page)
	}
	if _, err := os.Stat(cfg.SessionDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no-persistence created disk state: %v", err)
	}
}

func TestSupervisorRecoversDurableOutboxWithoutExecution(t *testing.T) {
	p := &childRuntimeTestProvider{name: "no-recovery-execution", stream: func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
		t.Error("recovery unexpectedly ran model")
		return llm.StreamResult{}, errors.New("unexpected")
	}}
	a, cfg := newChildRuntimeTestAgent(t, p)
	id := newSessionID()
	fact := session.ChildRunRecorded{Version: 1, Child: protocol.ChildSession{SessionID: protocol.SessionID(id), ParentSessionID: protocol.SessionID(a.sessionID), RootSessionID: protocol.SessionID(a.sessionID), DelegationID: "delegation", Purpose: childPurposeTask, Run: protocol.RunView{ID: "crashed-run", Status: "running", WaitPolicy: "join"}}}
	if _, err := a.persistenceHandle().commitEvents("catalog-before-crash", fact); err != nil {
		t.Fatal(err)
	}
	store, err := session.OpenJSONLStore(cfg.SessionDir, id)
	if err != nil {
		t.Fatal(err)
	}
	fact.Child.Run.Status = "succeeded"
	fact.Child.Run.Output = "saved answer"
	fact.Child.Run.Usage.InputTokens = 7
	fact.DeliveryID = "child-result-crashed-run"
	_, err = store.Commit(session.Beginning, session.NewBatch(session.SessionCreated{SessionID: id, FormatVersion: session.SchemaVersion, Source: "task", ParentID: a.sessionID, CreatedAt: time.Now().UTC()}, fact))
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	s := newSessionSupervisor(a)
	r, err := s.lookup(protocol.SessionID(id))
	if err != nil {
		t.Fatal(err)
	}
	if r.fact.Child.Run.Status != "succeeded" || r.fact.Child.Run.Output != "saved answer" {
		t.Fatalf("outbox not recovered: %+v", r.fact)
	}
	r.mu.Lock()
	err = s.recordOutcome(r)
	if err == nil {
		err = s.recordOutcome(r)
	}
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if a.Usage().InputTokens != 7 {
		t.Fatal("usage not idempotent")
	}
	again := newSessionSupervisor(a)
	rr, _ := again.lookup(protocol.SessionID(id))
	if !rr.fact.UsageAccounted {
		t.Fatal("usage receipt not durable")
	}
}

func TestSupervisorOutputPagesReadFullScopedArtifact(t *testing.T) {
	a, _ := newChildRuntimeTestAgent(t, &childRuntimeTestProvider{name: "output"})
	full := strings.Repeat("中文 output\n", 9000)
	p := a.persistenceHandle()
	ref, err := p.Artifacts().Put([]byte(full))
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.commitEvents("saved-output", session.ToolFinished{TurnID: "turn", CallID: "call", Status: "success", RawOutput: &ref, Result: session.ToolResult{CallID: "call", Status: "success", Text: "preview"}})
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	var offset int64
	for {
		page, err := a.Sessions().ReadOutput(context.Background(), protocol.SessionID(a.sessionID), "tool:turn:call", offset, 1025)
		if err != nil {
			t.Fatal(err)
		}
		out.WriteString(page.Text)
		offset = page.Next
		if !page.More {
			break
		}
	}
	if out.String() != full {
		t.Fatal("full output truncated or UTF-8 page boundary corrupted")
	}
	if _, err := a.Sessions().ReadOutput(context.Background(), "../escape", "tool:turn:call", 0, 10); err == nil {
		t.Fatal("unscoped session accepted")
	}
}

func TestSupervisorTranscriptPaginationIsStableAndBounded(t *testing.T) {
	a, _ := newChildRuntimeTestAgent(t, &childRuntimeTestProvider{name: "pages"})
	events := make([]session.Event, 600)
	for i := range events {
		events[i] = session.InputQueued{InputID: fmt.Sprintf("input-%d", i), Text: fmt.Sprintf("item %d", i), Strategy: "followup"}
	}
	for start := 0; start < len(events); start += 200 {
		if _, err := a.persistenceHandle().commitEvents(fmt.Sprintf("many-inputs-%d", start), events[start:min(start+200, len(events))]...); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	var before uint64
	for {
		page, err := a.Sessions().ReadTranscript(context.Background(), protocol.SessionID(a.sessionID), before, 17)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) > 17 {
			t.Fatal("unbounded page")
		}
		for _, item := range page.Items {
			if seen[item.ID] {
				t.Fatal("duplicate item across pages")
			}
			seen[item.ID] = true
		}
		if !page.More {
			break
		}
		if before != 0 && page.Before >= before {
			t.Fatal("cursor did not advance")
		}
		before = page.Before
	}
	if len(seen) != 600 {
		t.Fatalf("pagination lost items: %d", len(seen))
	}
}

func TestSupervisorStopTreeJoinsAllTargets(t *testing.T) {
	started := make(chan struct{}, 2)
	p := &childRuntimeTestProvider{name: "stop-tree", stream: func(ctx context.Context, _ llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		return llm.StreamResult{}, ctx.Err()
	}}
	a, _ := newChildRuntimeTestAgent(t, p)
	for i := 0; i < 2; i++ {
		if _, err := a.supervisor.launch(a, a.rootCtx, tools.SubagentTask{Description: "block", WaitPolicy: "notify"}, childPurposeTask, fmt.Sprint(i), i); err != nil {
			t.Fatal(err)
		}
	}
	<-started
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := a.Sessions().Control(ctx, protocol.AgentControl{ID: "stop-tree", SessionID: protocol.SessionID(a.sessionID), Action: "stop_tree"})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := a.Sessions().ListChildren(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Run.Status != "cancelled" {
			t.Fatalf("run not cancelled: %+v", row.Run)
		}
	}
}

func TestTranscriptLiveToolEventUsesDurableStepIdentity(t *testing.T) {
	a, _ := newChildRuntimeTestAgent(t, &childRuntimeTestProvider{name: "transcript-event"})
	a.mu.Lock()
	a.turnSeq, a.stepSeq = 1, 2
	a.mu.Unlock()
	a.transcript.facts([]session.Event{session.ToolStarted{TurnID: "turn-1", StepID: "step-2", Call: session.ToolCall{CallID: "call", ToolID: "Read", Arguments: []byte(`{}`)}}})
	sub, err := a.Watch(context.Background(), protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	initial := <-sub.Updates()
	a.publishEvent(Event{Type: EventToolStart, Tool: &ToolEvent{ID: "call", Name: "Read"}})
	update := <-sub.Updates()
	if update.Event == nil || update.Event.Transcript == nil || update.Event.Transcript.ID != "tool:turn-1:step-2:call" {
		t.Fatalf("event did not join durable tool identity: %+v", update.Event)
	}
	if update.Cursor.StreamEpoch == "" || update.Cursor.StreamEpoch != initial.Cursor.StreamEpoch || update.Cursor.EventSeq <= initial.Cursor.EventSeq {
		t.Fatalf("invalid stream cursor: %+v -> %+v", initial.Cursor, update.Cursor)
	}
}

func TestSupervisorLateOldDeliveryCannotReplaceCurrentRunOnRecovery(t *testing.T) {
	a, _ := newChildRuntimeTestAgent(t, &childRuntimeTestProvider{name: "late-recovery"})
	id := protocol.SessionID(newSessionID())
	old := session.ChildRunRecorded{Version: 1, Child: protocol.ChildSession{SessionID: id, ParentSessionID: protocol.SessionID(a.sessionID), RootSessionID: protocol.SessionID(a.sessionID), DelegationID: "delegation", Purpose: "task", Run: protocol.RunView{ID: "old-run", Status: "queued", WaitPolicy: "notify"}}}
	next := old
	next.Child.Run.ID = "new-run"
	p := a.persistenceHandle()
	for i, fact := range []session.ChildRunRecorded{old, next} {
		if _, err := p.commitEvents(fmt.Sprint("queue-", i), fact); err != nil {
			t.Fatal(err)
		}
	}
	old.Child.Run.Status = "succeeded"
	old.Delivered, old.UsageAccounted = true, true
	if _, err := p.commitEvents("late-delivered", old); err != nil {
		t.Fatal(err)
	}
	s := newSessionSupervisor(a)
	r, err := s.lookup(id)
	if err != nil {
		t.Fatal(err)
	}
	if r.fact.Child.Run.ID != "new-run" {
		t.Fatalf("late receipt rolled back current run: %+v", r.fact.Child.Run)
	}
	if !s.runs["old-run"].fact.Delivered {
		t.Fatal("older delivery receipt lost")
	}
}

func TestSupervisorRejectsExpiredStepInsteadOfBorrowingClosedMCP(t *testing.T) {
	a, cfg := newChildRuntimeTestAgent(t, &childRuntimeTestProvider{name: "expired-step"})
	lease := a.mcp.AcquireStep(a.registry)
	lease.Close()
	a.mu.Lock()
	a.busy = true
	a.turnSeq = 1
	a.childStep = &childStepSnapshot{mcpLease: lease, toolLease: lease.Tools, cfg: cfg, binding: a.activeBinding, models: a.models.Freeze(), turn: 1, registryNames: a.registry.Names()}
	a.mu.Unlock()
	_, _, _, _, _, err := a.supervisor.prepare(a, childPurposeTask, "")
	if err == nil {
		t.Fatal("closed generation was borrowed by continuation")
	}
	a.mu.Lock()
	a.busy = false
	a.childStep = nil
	a.mu.Unlock()
}

func TestTranscriptRestoredToolResultSettlesQueuedCall(t *testing.T) {
	state := &transcriptState{}
	state.facts([]session.Event{
		session.AssistantCommitted{TurnID: "turn-1", StepID: "step-1", Message: session.Message{MessageID: "assistant", Role: "assistant", Content: []session.ContentBlock{{Kind: session.ContentToolCall, ToolCall: &session.ToolCall{CallID: "call", ToolID: "Read", Arguments: []byte(`{}`)}}}}},
		session.AssistantCommitted{TurnID: "turn-1", StepID: "step-1", Message: session.Message{MessageID: "result", Role: "tool", Content: []session.ContentBlock{{Kind: session.ContentToolResult, ToolResult: &session.ToolResult{CallID: "call", Status: "success", Text: "restored output"}}}}},
	})
	items, _ := state.snapshot()
	if len(items) != 1 || items[0].Status != "success" || items[0].Text != "restored output" {
		t.Fatalf("restored tool left queued: %+v", items)
	}
}
