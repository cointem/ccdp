package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/tools"
)

func journalGoal(t *testing.T, a *Agent) *protocol.Goal {
	t.Helper()
	a.services.registry = tools.NewRegistry()
	now := time.Now().UTC()
	goal := &protocol.Goal{ID: "accounted-goal", Objective: "finish", Status: protocol.GoalActive, CreatedAt: now, UpdatedAt: now}
	if err := a.persistGoal(goal); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.conversation.durable.Goal = protocol.CloneGoal(goal)
	a.mu.Unlock()
	return goal
}

func TestGoalRequestAccountingSharesAttemptTransaction(t *testing.T) {
	a, p, _ := newJournalAgent(t, true)
	goal := journalGoal(t, a)
	attempt, err := a.recordPreparedRequest(context.Background(), "main", "provider", "https://example.test", journalRequest())
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Manifest.GoalID != goal.ID || attempt.Manifest.GoalSessionID != a.SessionID() {
		t.Fatalf("missing goal binding: %+v", attempt.Manifest)
	}
	if err := a.finishRequestAttempt(attempt, llm.StreamResult{PromptTokens: 100, CachedTokens: 60, CompletionTok: 12}, nil); err != nil {
		t.Fatal(err)
	}
	records, err := p.Read(session.Beginning)
	if err != nil {
		t.Fatal(err)
	}
	var attemptTx, goalTx string
	for _, record := range records {
		switch event := record.Event.(type) {
		case *session.AttemptFinished:
			attemptTx = record.TransactionID
			if event.Attempt.GoalTokens != 52 {
				t.Fatalf("tokens=%d", event.Attempt.GoalTokens)
			}
		case *session.GoalChanged:
			if event.AccountingID != "" {
				goalTx = record.TransactionID
			}
		}
	}
	if attemptTx == "" || goalTx != attemptTx {
		t.Fatalf("non-atomic accounting attempt=%q goal=%q", attemptTx, goalTx)
	}
	if err := a.recoverGoalAccounting(); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	got := protocol.CloneGoal(a.conversation.durable.Goal)
	a.mu.Unlock()
	if got.TokensUsed != 52 || got.ModelCalls != 1 {
		t.Fatalf("recovery double counted: %+v", got)
	}
}

func TestGoalRequestKeepsPreparedOwnerAfterGoalReplacement(t *testing.T) {
	a, _, _ := newJournalAgent(t, true)
	journalGoal(t, a)
	attempt, err := a.recordPreparedRequest(context.Background(), "main", "provider", "https://example.test", journalRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.changeGoal(func(g *protocol.Goal) (*protocol.Goal, error) { g.ID = "replacement"; return g, nil }); err != nil {
		t.Fatal(err)
	}
	if err := a.finishRequestAttempt(attempt, llm.StreamResult{PromptTokens: 10, CompletionTok: 2}, nil); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	got := protocol.CloneGoal(a.conversation.durable.Goal)
	a.mu.Unlock()
	if got.ID != "replacement" || got.TokensUsed != 0 || got.ModelCalls != 0 {
		t.Fatalf("old request charged replacement: %+v", got)
	}
}

func TestGoalAccountingRecoversNestedChildCompletionOnce(t *testing.T) {
	a, p, cfg := newJournalAgent(t, false)
	goal := journalGoal(t, a)
	childID, grandID := "goal-child", "goal-grandchild"
	edge := func(parent, child string) session.ChildRunRecorded {
		return session.ChildRunRecorded{GoalSessionID: a.SessionID(), GoalID: goal.ID, Version: 1, Child: protocol.ChildSession{SessionID: protocol.SessionID(child), ParentSessionID: protocol.SessionID(parent), RootSessionID: protocol.SessionID(a.SessionID()), DelegationID: protocol.DelegationID("delegate-" + child), Purpose: "task", Run: protocol.RunView{ID: protocol.RunID("run-" + child), Status: "unknown", WaitPolicy: "join"}}}
	}
	if _, err := p.Commit(session.Batch{TransactionID: "child-edge", Events: []session.Event{edge(a.SessionID(), childID)}}); err != nil {
		t.Fatal(err)
	}
	child, err := openSessionPersistence(cfg, childID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := child.Commit(session.Batch{TransactionID: "grand-edge", Events: []session.Event{edge(childID, grandID)}}); err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	grand, err := openSessionPersistence(cfg, grandID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	manifest := session.RequestManifest{RequestID: "child-request", SessionID: grandID, GoalSessionID: a.SessionID(), GoalID: goal.ID, Model: "test", Provider: "test", AdapterVersion: "test", Digest: "test", Messages: []session.Message{{MessageID: "prompt", Role: "user", Content: []session.ContentBlock{{Kind: session.ContentText, Text: "work"}}}}}
	attempt := session.Attempt{AttemptID: "child-attempt", RequestID: manifest.RequestID, SessionID: grandID, Model: "test", Provider: "test", Outcome: "success", GoalTokens: 87, GoalTokensEstimated: true}
	if _, err := grand.Commit(session.Batch{TransactionID: "finished", Events: []session.Event{session.RequestPrepared{Manifest: manifest}, session.AttemptFinished{Attempt: attempt}}}); err != nil {
		t.Fatal(err)
	}
	if err := grand.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := a.recoverGoalAccounting(); err != nil {
			t.Fatal(err)
		}
	}
	a.mu.Lock()
	got := protocol.CloneGoal(a.conversation.durable.Goal)
	a.mu.Unlock()
	if got.TokensUsed != 87 || got.ModelCalls != 1 || !got.TokensEstimated {
		t.Fatalf("recovery accounting: %+v", got)
	}
	snap, err := p.snapshotFromOwnedStore(a.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	if !snap.GoalAccountingIDs[grandID+":"+attempt.AttemptID] {
		t.Fatal("accounting id not durable")
	}
}

func TestGoalEstimatesToolArgumentsAndPreservesInternalSource(t *testing.T) {
	base, _ := goalAttemptTokens(10, llm.StreamResult{})
	withTool, estimated := goalAttemptTokens(10, llm.StreamResult{ToolCalls: []llm.ToolCall{{ID: "tool", Function: llm.Function{Name: "Write", Arguments: `{"content":"nontrivial output"}`}}}})
	if !estimated || withTool <= base {
		t.Fatalf("tool estimate=%d base=%d", withTool, base)
	}
	message := messages.Message{ID: "internal", Role: messages.RoleUser, Content: "continue", InternalSource: "goal"}
	stored, err := messageToSession(message)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := session.DecodeMessage(stored)
	if err != nil {
		t.Fatal(err)
	}
	if restored.InternalSource != "goal" {
		t.Fatal("internal source lost")
	}
	events, err := memoryResumeEvents(SessionSnapshot{ID: "internal", CreatedAt: time.Now(), History: []messages.Message{message}})
	if err != nil {
		t.Fatal(err)
	}
	store := session.NewMemoryStore()
	defer store.Close()
	if _, err := store.Commit(session.Beginning, session.Batch{TransactionID: "resume", Events: events}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := SessionSnapshotProjection(store, "internal")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.History) != 1 || snapshot.History[0].InternalSource != "goal" {
		t.Fatalf("memory resume lost internal source: %+v", snapshot.History)
	}
}

func TestGoalRecoveryIgnoresUnrelatedChildrenAndBlocksOnlyAffectedGoal(t *testing.T) {
	for _, related := range []bool{false, true} {
		name := "unrelated"
		if related {
			name = "related"
		}
		t.Run(name, func(t *testing.T) {
			a, p, cfg := newJournalAgent(t, false)
			goal := journalGoal(t, a)
			boundGoal := "previous-goal"
			if related {
				boundGoal = goal.ID
			}
			childID := "damaged-child"
			fact := session.ChildRunRecorded{Version: 1, GoalSessionID: a.SessionID(), GoalID: boundGoal, Child: protocol.ChildSession{SessionID: protocol.SessionID(childID), ParentSessionID: protocol.SessionID(a.SessionID()), RootSessionID: protocol.SessionID(a.SessionID()), DelegationID: "damaged-delegation", Purpose: "task", Run: protocol.RunView{ID: "damaged-run", Status: "succeeded", WaitPolicy: "join"}}}
			if _, err := p.Commit(session.Batch{TransactionID: "child", Events: []session.Event{fact}}); err != nil {
				t.Fatal(err)
			}
			child, err := openSessionPersistence(cfg, childID, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			if err := child.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cfg.SessionDir, childID, "events.v1.jsonl"), []byte("invalid log\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := a.recoverGoalAccounting(); err != nil {
				t.Fatalf("child damage escaped to parent: %v", err)
			}
			a.mu.Lock()
			got := protocol.CloneGoal(a.conversation.durable.Goal)
			a.mu.Unlock()
			if related && (got.Status != protocol.GoalBlocked || !strings.Contains(got.Reason, "recovery incomplete")) {
				t.Fatalf("related damage did not block goal: %+v", got)
			}
			if !related && got.Status != protocol.GoalActive {
				t.Fatalf("unrelated history affected current goal: %+v", got)
			}
			if err := a.persistenceFailure(); err != nil {
				t.Fatalf("healthy parent journal poisoned: %v", err)
			}
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
			snapshot, err := LoadSession(cfg.SessionDir, a.SessionID())
			if err != nil {
				t.Fatal(err)
			}
			resumed, err := Resume(cfg, snapshot)
			if err != nil {
				t.Fatalf("parent no longer opens: %v", err)
			}
			defer resumed.Close()
			resumed.mu.Lock()
			restored := protocol.CloneGoal(resumed.conversation.durable.Goal)
			resumed.mu.Unlock()
			want := protocol.GoalPaused
			if related {
				want = protocol.GoalBlocked
			}
			if restored.Status != want {
				t.Fatalf("restored status=%s want=%s", restored.Status, want)
			}
		})
	}
}

func TestGoalResumeRechecksDamagedChildAccounting(t *testing.T) {
	var calls atomic.Int32
	provider := &childRuntimeTestProvider{name: "goal-recovery-resume"}
	provider.stream = func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
		calls.Add(1)
		return goalCompleteTestResponse(), nil
	}
	a, cfg := newChildRuntimeTestAgent(t, provider)
	now := time.Now().UTC()
	goal, err := a.changeGoal(func(*protocol.Goal) (*protocol.Goal, error) {
		return &protocol.Goal{ID: "recovery-goal", Objective: "finish after repairing accounting", Status: protocol.GoalPaused, CreatedAt: now, UpdatedAt: now}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	childID := "recovery-child"
	fact := session.ChildRunRecorded{Version: 1, GoalSessionID: a.SessionID(), GoalID: goal.ID, Child: protocol.ChildSession{SessionID: protocol.SessionID(childID), ParentSessionID: protocol.SessionID(a.SessionID()), RootSessionID: protocol.SessionID(a.SessionID()), DelegationID: "recovery-delegation", Purpose: "task", Run: protocol.RunView{ID: "recovery-run", Status: "succeeded", WaitPolicy: "join"}}}
	if _, err := a.persistenceHandle().Commit(session.Batch{TransactionID: "child", Events: []session.Event{fact}}); err != nil {
		t.Fatal(err)
	}
	child, err := openSessionPersistence(&cfg, childID, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.SessionDir, childID, "events.v1.jsonl")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("invalid log\n"), 0600); err != nil {
		t.Fatal(err)
	}
	go a.Run()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	receipt, err := a.Submit(ctx, protocol.Command{ID: "resume-damaged", SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandGoal, Goal: &protocol.GoalCommand{Action: "resume"}})
	if err != nil || !receipt.Rejected() {
		t.Fatalf("damaged accounting resumed: receipt=%+v err=%v", receipt, err)
	}
	if receipt.Error == nil || !strings.Contains(receipt.Error.Message, childID) {
		t.Fatalf("resume rejection lacks accounting cause: %+v", receipt)
	}
	view := waitGoalTestState(t, a, protocol.GoalBlocked)
	if !strings.Contains(view.Goal.Reason, childID) || calls.Load() != 0 {
		t.Fatalf("blocked goal lacks cause or ran provider: goal=%+v calls=%d", view.Goal, calls.Load())
	}
	if err := a.persistenceFailure(); err != nil {
		t.Fatalf("child damage poisoned healthy parent: %v", err)
	}
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	submitGoalTestCommand(t, a, "resume-repaired", "resume", "", nil)
	waitGoalTestState(t, a, protocol.GoalComplete)
	if calls.Load() != 1 {
		t.Fatalf("provider calls=%d want=1", calls.Load())
	}
}
