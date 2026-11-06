package agent

import (
	"context"
	"fmt"
	"testing"

	"ccdp/internal/llm"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/session/archive"
)

// Reopen the actual writer and projection, dropping all process-local request
// and reservation state just as a new process does after a crash.
func reopenAccountingAgent(t *testing.T, old *Agent) *Agent {
	t.Helper()
	if err := old.persistenceHandle().Close(); err != nil {
		t.Fatal(err)
	}
	old.Close()
	snapshot, err := archive.Load(old.services.cfg.SessionDir, old.conversation.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	a, err := Resume(old.services.cfg, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

func TestPreparedRequestRecoveryKeepsReservationAndGoalChargeOnce(t *testing.T) {
	a, _, _ := newJournalAgent(t, false)
	journalGoal(t, a)

	req := journalRequest()
	attempt, err := a.recordPreparedRequest(context.Background(), "main", "provider", "", req, testRequestMetadata(a))
	if err != nil {
		t.Fatal(err)
	}
	bound := *attempt.Manifest.ReservedUsage
	if bound.InputTokens <= 0 || bound.OutputTokens <= 0 || bound.TotalTokens != bound.InputTokens+bound.OutputTokens {
		t.Fatalf("invalid durable bound: %+v", bound)
	}
	if _, err := a.changeGoal(func(goal *protocol.Goal) (*protocol.Goal, error) {
		goal.TokenBudget = bound.TotalTokens
		return goal, nil
	}); err != nil {
		t.Fatal(err)
	}
	// Caller-visible metadata cannot rewrite recovery's captured token bound.
	attempt.Manifest.ReservedUsage.InputTokens = 0

	committed, err := a.persistenceHandle().CommittedRequestManifest(attempt.RequestID)
	if err != nil || committed.ReservedUsage.InputTokens != bound.InputTokens {
		t.Fatalf("mutable committed bound: %+v error=%v", committed, err)
	}

	committed.ReservedUsage.InputTokens = 0
	a = reopenAccountingAgent(t, a)
	for i := 0; i < 2; i++ {
		if err := a.reconcileDescendantUsage(); err != nil {
			t.Fatal(err)
		}
		ready, err := a.recoverGoalAccountingState()
		if err != nil || !ready {
			t.Fatalf("recovery incomplete: ready=%t error=%v", ready, err)
		}
		if got := a.Usage(); got.InputTokens != int(bound.InputTokens) || got.OutputTokens != int(bound.OutputTokens) || got.TurnCount != 1 {
			t.Fatalf("recovery %d usage=%+v bound=%+v", i, got, bound)
		}
		if goal := a.conversation.durable.Goal; goal.TokensUsed != bound.TotalTokens || goal.ModelCalls != 1 || !goal.TokensEstimated || goal.Status != protocol.GoalBudgetLimited {
			t.Fatalf("recovered goal=%+v", goal)
		}
		a = reopenAccountingAgent(t, a)
	}
	records, err := a.persistenceHandle().Read(session.Beginning)
	if err != nil {
		t.Fatal(err)
	}
	var finished, charged int
	var finishTx string
	for _, record := range records {
		switch fact := record.Event.(type) {
		case *session.AttemptFinished:
			finished++
			finishTx = record.TransactionID
			if fact.Attempt.Outcome != "unknown" || !fact.Attempt.UsageEstimated || !fact.Attempt.GoalTokensEstimated {
				t.Fatalf("recovery advertised actual usage: %+v", fact.Attempt)
			}
		case *session.GoalChanged:
			if fact.AccountingID != "" {
				charged++
				if record.TransactionID != finishTx {
					t.Fatal("recovery completion and goal charge are not atomic")
				}
			}
		}
	}
	if finished != 1 || charged != 1 {
		t.Fatalf("duplicate recovery facts: finished=%d goal=%d", finished, charged)
	}
}

func recoveryChildEdge(rootID, goalID, parentID, childID string) session.ChildRunRecorded {
	return session.ChildRunRecorded{Version: 1, GoalSessionID: rootID, GoalID: goalID, Child: protocol.ChildSession{
		SessionID: protocol.SessionID(childID), ParentSessionID: protocol.SessionID(parentID), RootSessionID: protocol.SessionID(rootID),
		DelegationID: protocol.DelegationID("delegate-" + childID), Purpose: "task", Run: protocol.RunView{ID: protocol.RunID("run-" + childID), Status: "unknown", WaitPolicy: "join"},
	}}
}

func TestPreparedNestedChildRecoveryTransfersTokensAndGoalOnce(t *testing.T) {
	for _, goalFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "usage-first", true: "goal-first"}[goalFirst], func(t *testing.T) {
			root, _, cfg := newJournalAgent(t, false)
			goal := journalGoal(t, root)
			parent := root
			var totalTokens int64
			for _, id := range []string{"pending-child", "pending-grandchild"} {
				if _, err := parent.persistenceHandle().CommitEvents("edge-"+id, recoveryChildEdge(root.conversation.sessionID, goal.ID, parent.conversation.sessionID, id)); err != nil {
					t.Fatal(err)
				}
				childCfg := cfg.Clone()
				childCfg.SessionID = id
				child, err := testNewWithOptions(t, &childCfg, Options{EffectiveConfigFrozen: true, Purpose: childPurposeTask, ParentSessionID: parent.SessionID(), Supervisor: root.services.supervisor, goalOwner: root, goalOwnerID: goal.ID})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(child.Close)
				request, err := child.recordPreparedRequest(context.Background(), "task", "provider", "", journalRequest(), testRequestMetadata(child))
				if err != nil {
					t.Fatal(err)
				}
				totalTokens += request.Manifest.ReservedUsage.TotalTokens
				if parent != root {
					if err := parent.persistenceHandle().Close(); err != nil {
						t.Fatal(err)
					}
				}
				parent = child
			}
			if err := parent.persistenceHandle().Close(); err != nil {
				t.Fatal(err)
			}
			root = reopenAccountingAgent(t, root)
			for i := 0; i < 2; i++ {
				if goalFirst {
					if ready, err := root.recoverGoalAccountingState(); err != nil || !ready {
						t.Fatalf("ready=%t error=%v", ready, err)
					}
				}
				if err := root.reconcileDescendantUsage(); err != nil {
					t.Fatal(err)
				}
				if ready, err := root.recoverGoalAccountingState(); err != nil || !ready {
					t.Fatalf("ready=%t error=%v", ready, err)
				}
				if got := root.Usage(); int64(got.InputTokens+got.OutputTokens) != totalTokens || got.TurnCount != 2 || got.Cache.InputTokens != 0 || got.Cache.UnknownHistory {
					t.Fatalf("recovery %d shared usage=%+v want=%d tokens", i, got, totalTokens)
				}
				if got := root.conversation.durable.Goal; got.TokensUsed != totalTokens || got.ModelCalls != 2 || !got.TokensEstimated {
					t.Fatalf("recovery %d shared goal=%+v want=%d", i, got, totalTokens)
				}
				root = reopenAccountingAgent(t, root)
			}
		})
	}
}

func TestAccountingRecoverySkipsCurrentRootAndChildRequests(t *testing.T) {
	root, _, cfg := newJournalAgent(t, false)
	goal := journalGoal(t, root)
	if _, err := root.recordPreparedRequest(context.Background(), "main", "provider", "", journalRequest(), testRequestMetadata(root)); err != nil {
		t.Fatal(err)
	}
	childID := "live-child"
	if _, err := root.persistenceHandle().CommitEvents("edge", recoveryChildEdge(root.conversation.sessionID, goal.ID, root.conversation.sessionID, childID)); err != nil {
		t.Fatal(err)
	}
	childCfg := cfg.Clone()
	childCfg.SessionID = childID
	child, err := testNewWithOptions(t, &childCfg, Options{EffectiveConfigFrozen: true, Purpose: childPurposeTask, ParentSessionID: root.SessionID(), Supervisor: root.services.supervisor, goalOwner: root, goalOwnerID: goal.ID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(child.Close)
	if _, err := child.recordPreparedRequest(context.Background(), "task", "provider", "", journalRequest(), testRequestMetadata(child)); err != nil {
		t.Fatal(err)
	}
	root.services.supervisor = testSupervisor(root, &managedRun{agent: child, fact: session.ChildRunRecorded{Child: protocol.ChildSession{SessionID: protocol.SessionID(childID)}}})
	if err := root.reconcileDescendantUsage(); err != nil {
		t.Fatal(err)
	}
	if ready, err := root.recoverGoalAccountingState(); err != nil || !ready {
		t.Fatalf("live requests blocked recovery: ready=%t error=%v", ready, err)
	}
	if root.conversation.durable.Goal.TokensUsed != 0 || root.conversation.durable.Goal.ModelCalls != 0 {
		t.Fatalf("in-flight requests were charged: usage=%+v goal=%+v", root.Usage(), root.conversation.durable.Goal)
	}
}

func TestRecoveryIdentitiesIncludeSourceSession(t *testing.T) {
	root, _, _ := newJournalAgent(t, true)
	ids := map[string]bool{}
	for _, sessionID := range []string{"child-a", "child-b"} {
		manifest := session.RequestManifest{SessionID: sessionID, RequestID: "same-local-request", Provider: "provider", Model: "model", ReservedUsage: &session.Usage{InputTokens: 100, OutputTokens: 200, TotalTokens: 300}}
		entries, err := root.requestAccountingAttempts(sessionID, []session.Record{{Event: &session.RequestPrepared{Manifest: manifest}}}, nil)
		if err != nil || len(entries) != 1 {
			t.Fatalf("entries=%+v error=%v", entries, err)
		}
		if ids[entries[0].attempt.AttemptID] {
			t.Fatal("different source sessions received the same recovery identity")
		}
		ids[entries[0].attempt.AttemptID] = true
		if err := root.accountSharedAttempt(entries[0].attempt); err != nil {
			t.Fatal(err)
		}
	}
	if got := root.Usage(); got.TurnCount != 2 || got.InputTokens != 200 || got.OutputTokens != 400 {
		t.Fatalf("source identity collision lost a charge: %+v", got)
	}
}

func TestGoalAccountingEstimatesOnlyMissingUsageFields(t *testing.T) {
	partial := llm.StreamResult{PromptReported: true, PromptTokens: 100, CachedTokens: 20, Text: "partial output with several tokens"}
	tokens, estimated := goalAttemptTokens(999, partial)
	if !estimated || tokens <= 80 || tokens >= 999 {
		t.Fatalf("missing output was omitted or input discarded: tokens=%d estimated=%t", tokens, estimated)
	}
	partial.CompletionReported = true
	tokens, estimated = goalAttemptTokens(999, partial)
	if estimated || tokens != 80 {
		t.Fatalf("explicit output zero estimated: tokens=%d estimated=%t", tokens, estimated)
	}
	tokens, estimated = goalAttemptTokens(999, llm.StreamResult{PromptReported: true, CompletionReported: true})
	if estimated || tokens != 0 {
		t.Fatalf("explicit zeros estimated: tokens=%d estimated=%t", tokens, estimated)
	}
}

func TestChildUsageBeforeFirstOwnRequestDoesNotInventUnknownCacheHistory(t *testing.T) {
	a, p, _ := newJournalAgent(t, true)
	if err := a.accountSharedAttempt(session.Attempt{AttemptID: "child", SessionID: "child", Usage: session.Usage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120, TurnCount: 1}}); err != nil {
		t.Fatal(err)
	}
	attempt, err := a.recordPreparedRequest(context.Background(), "main", "provider", "", journalRequest(), testRequestMetadata(a))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.finishRequestAttempt(attempt, llm.StreamResult{PromptTokens: 100, CompletionTok: 10, CachedTokens: 80, CacheReported: true}, nil); err != nil {
		t.Fatal(err)
	}
	if got := a.Usage().Cache; got.UnknownHistory || got.InputTokens != 100 || got.ReportedInputTokens != 100 || got.CachedTokens != 80 {
		t.Fatalf("first own request inherited child cache history: %+v", got)
	}
	snapshot, err := archive.FromStore(p, a.conversation.sessionID)
	if err != nil || snapshot.Usage.Cache.UnknownHistory {
		t.Fatalf("incremental replay cache=%+v error=%v", snapshot.Usage.Cache, err)
	}
	records, err := p.Read(session.Beginning)
	if err != nil {
		t.Fatal(err)
	}
	store := session.NewMemoryStore()
	defer store.Close()
	for _, record := range records {
		// Exercise AttemptFinished replay without its absolute UsageChanged,
		// which could otherwise mask the same wrong aggregate-count inference.
		if _, ok := record.Event.(*session.UsageChanged); ok && record.TransactionID == "attempt-finished-"+attempt.AttemptID {
			continue
		}
		if _, err := store.Commit(store.CurrentCursor(), session.Batch{TransactionID: fmt.Sprintf("%s-%d", record.TransactionID, record.Seq), Events: []session.Event{record.Event}}); err != nil {
			t.Fatal(err)
		}
	}
	replayed, err := archive.FromStore(store, a.conversation.sessionID)
	if err != nil || replayed.Usage.Cache.UnknownHistory {
		t.Fatalf("full replay invented unknown cache history: %+v error=%v", replayed.Usage.Cache, err)
	}
}

func TestTokenAccountingRespectsExplicitZeroUsage(t *testing.T) {
	a, p, _ := newJournalAgent(t, true)

	req := journalRequest()
	attempt, err := a.recordPreparedRequest(context.Background(), "main", "provider", "", req, testRequestMetadata(a))
	if err != nil {
		t.Fatal(err)
	}

	if err := a.finishRequestAttempt(attempt, llm.StreamResult{PromptReported: true, CompletionReported: true}, nil); err != nil {
		t.Fatal(err)
	}
	if a.Usage().InputTokens != 0 || a.Usage().OutputTokens != 0 {
		t.Fatalf("explicit zero charged reservation: %+v", a.Usage())
	}
	rows, err := p.Read(session.Beginning)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if fact, ok := row.Event.(*session.AttemptFinished); ok && fact.Attempt.UsageEstimated {
			t.Fatal("explicit zero advertised as an estimate")
		}
	}
}

func TestProviderRejectionConsumesNoGoalTokens(t *testing.T) {
	a, _, _ := newJournalAgent(t, true)
	journalGoal(t, a)

	req := journalRequest()
	attempt, err := a.recordPreparedRequest(context.Background(), "main", "provider", "", req, testRequestMetadata(a))
	if err != nil {
		t.Fatal(err)
	}

	if err := a.finishRequestAttempt(attempt, llm.StreamResult{}, &llm.HTTPRejection{StatusCode: 400, Message: "invalid request"}); err != nil {
		t.Fatal(err)
	}
	if a.conversation.durable.Goal.TokensUsed != 0 || a.conversation.durable.Goal.TokensEstimated || a.conversation.durable.Goal.ModelCalls != 1 {
		t.Fatalf("known rejection consumed goal budget: usage=%+v goal=%+v", a.Usage(), a.conversation.durable.Goal)
	}
}
