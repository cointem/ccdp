package agent

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"ccdp/internal/llm"
	"ccdp/internal/plugin"
	"ccdp/internal/session"
)

// The returned request has been committed in the child's journal but has not
// been transferred to its root. This is the boundary a process/session switch
// can observe between local and shared settlement.
func commitMemoryAccountingRequest(t *testing.T, p *sessionPersistence, rootID, goalID, childID, requestID string, input, output int64, finished bool) (session.RequestManifest, session.Attempt) {
	t.Helper()
	manifest := session.RequestManifest{
		SessionID: childID, RequestID: requestID, GoalSessionID: rootID, GoalID: goalID,
		Model: "model", Provider: "provider", AdapterVersion: "test-v1", Digest: requestID,
		Messages:      []session.Message{{MessageID: requestID + "-message", Role: "user", Content: []session.ContentBlock{{Kind: session.ContentText, Text: "work"}}}},
		ReservedUsage: &session.Usage{InputTokens: input, OutputTokens: output, TotalTokens: input + output},
	}
	attempt := session.Attempt{
		SessionID: childID, RequestID: requestID, AttemptID: requestID + "-attempt", Provider: manifest.Provider, Model: manifest.Model,
		Outcome: "success", GoalTokens: input + output, Usage: session.Usage{InputTokens: input, OutputTokens: output, TotalTokens: input + output, TurnCount: 1},
	}
	facts := []session.Event{session.RequestPrepared{Manifest: manifest}}
	if finished {
		facts = append(facts, session.AttemptFinished{Attempt: attempt})
	}
	if _, err := p.CommitEvents(requestID, facts...); err != nil {
		t.Fatal(err)
	}
	return manifest, attempt
}

func retainAccountingRun(t *testing.T, root *Agent, p *sessionPersistence, fact session.ChildRunRecorded) {
	t.Helper()
	records, artifacts, err := p.ReadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := copyMemoryArtifacts(records, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	close(done)
	root.services.supervisor.directory.Restore(fact.Child, &managedRun{parent: root, fact: fact, memory: records, memoryBlobs: blobs, done: done}, true)
}

func TestMemoryAccountingRecoversLatestPrefixesAndNestedChildrenOnce(t *testing.T) {
	a, _ := sessionRepairAgent(t, true)
	goal := journalGoal(t, a)
	rootID := a.SessionID()
	child, err := openSessionPersistence(a.services.cfg, "memory-account-child", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	edge := recoveryChildEdge(rootID, goal.ID, rootID, "memory-account-child")
	if _, err := a.persistenceHandle().CommitEvents("child-edge", edge); err != nil {
		t.Fatal(err)
	}
	manifest, attempt := commitMemoryAccountingRequest(t, child, rootID, goal.ID, "memory-account-child", "first-request", 7, 3, true)
	retainAccountingRun(t, a, child, edge)
	// One charge already made it to the parent before the switch.
	if err := a.accountSharedAttempt(attempt); err != nil {
		t.Fatal(err)
	}
	if err := a.accountGoalFinished(manifest, attempt, false); err != nil {
		t.Fatal(err)
	}
	grand, err := openSessionPersistence(a.services.cfg, "memory-account-grandchild", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	defer grand.Close()
	grandEdge := recoveryChildEdge(rootID, goal.ID, "memory-account-child", "memory-account-grandchild")
	if _, err := child.CommitEvents("grandchild-edge", grandEdge); err != nil {
		t.Fatal(err)
	}
	commitMemoryAccountingRequest(t, grand, rootID, goal.ID, "memory-account-grandchild", "abandoned-request", 11, 4, false)
	retainAccountingRun(t, a, grand, grandEdge)
	// Continuation logs are full prefixes. Reading both would double the first
	// request; reading only the first would miss both later work and the edge.
	commitMemoryAccountingRequest(t, child, rootID, goal.ID, "memory-account-child", "second-request", 9, 1, true)
	edge.Child.Run.ID = "second-child-run"
	if _, err := a.persistenceHandle().CommitEvents("second-child-edge", edge); err != nil {
		t.Fatal(err)
	}
	retainAccountingRun(t, a, child, edge)
	// The newest run may only have an admission and no writer yet.
	queued := edge
	queued.Child.Run.ID, queued.Child.Run.Status = "queued-child-run", "queued"
	if _, err := a.persistenceHandle().CommitEvents("queued-child-edge", queued); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	close(done)
	a.services.supervisor.directory.Restore(queued.Child, &managedRun{parent: a, fact: queued, done: done}, true)
	oldGoalChild, err := openSessionPersistence(a.services.cfg, "old-goal-child", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	defer oldGoalChild.Close()
	oldEdge := recoveryChildEdge(rootID, "previous-goal", rootID, "old-goal-child")
	if _, err := a.persistenceHandle().CommitEvents("old-goal-edge", oldEdge); err != nil {
		t.Fatal(err)
	}
	commitMemoryAccountingRequest(t, oldGoalChild, rootID, "previous-goal", "old-goal-child", "old-goal-request", 5, 2, true)
	retainAccountingRun(t, a, oldGoalChild, oldEdge)

	for reopen := 0; reopen < 2; reopen++ {
		next, err := a.OpenSession(rootID)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(next.Close)
		a = next
		if got := a.Usage(); got.InputTokens != 32 || got.OutputTokens != 10 || got.TurnCount != 4 {
			t.Fatalf("reopen %d aggregate=%+v; want 32 input, 10 output, 4 requests", reopen, got)
		}
		if got := a.goalSnapshot(); got.TokensUsed != 35 || got.ModelCalls != 3 || !got.TokensEstimated {
			t.Fatalf("reopen %d goal lost bindings or counted twice: %+v", reopen, got)
		}
		rows, _, err := a.services.supervisor.readRecordedSessionRecords(context.Background(), "memory-account-grandchild")
		if err != nil || len(rows) == 0 {
			t.Fatalf("reopen %d lost nested journal: records=%d err=%v", reopen, len(rows), err)
		}
	}
	if _, err := os.Stat(a.services.cfg.SessionDir); !os.IsNotExist(err) {
		t.Fatalf("memory accounting created session storage: %v", err)
	}
}

type accountingSnapshotStore struct {
	session.Store
	once  sync.Once
	after func()
}

func (s *accountingSnapshotStore) Read(cursor session.Cursor) ([]session.Record, error) {
	records, err := s.Store.Read(cursor)
	s.once.Do(s.after)
	return records, err
}

func TestMemoryAccountingRecoversChildSettlingAfterParentSnapshot(t *testing.T) {
	cfg := isolatedTestConfig(t, "snapshot-child-accounting")
	entered, release := make(chan struct{}), make(chan struct{})
	provider := &childRuntimeTestProvider{name: cfg.Model, stream: func(ctx context.Context, _ llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return llm.StreamResult{}, ctx.Err()
		}
		return llm.StreamResult{Text: "answer", PromptTokens: 7, CompletionTok: 3, FinishReason: "stop"}, nil
	}}
	models := plugin.NewModelRegistry()
	models.Register(provider)
	models.Route(cfg.Model, provider.Name())
	a, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true, ProviderRegistry: models})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	journalGoal(t, a)
	r, err := a.services.supervisor.launch(a, a.Context(), childTask{Description: "charged work"}, childPurposeTask, "snapshot-child")
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	r.mu.Lock()
	child := r.agent
	r.mu.Unlock()
	backend := journalBackend(t, a.persistenceHandle())
	backend.mu.Lock()
	backend.store = &accountingSnapshotStore{Store: backend.store, after: func() {
		// ReadSnapshot holds the parent writer lock. Let the child finish its
		// local commit while that lock blocks transfer into the copied prefix.
		close(release)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			rows, _, err := child.ReadRunRecords(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			for _, row := range rows {
				if _, ok := row.Event.(*session.AttemptFinished); ok {
					return
				}
			}
			time.Sleep(time.Millisecond)
		}
		t.Error("child did not finish local settlement")
	}}
	backend.mu.Unlock()
	reopened, err := a.OpenSession(a.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.Close)
	waitManaged(t, r)
	for _, owner := range []*Agent{a, reopened} {
		if got := owner.Usage(); got.InputTokens != 7 || got.OutputTokens != 3 || got.TurnCount != 1 {
			t.Fatalf("snapshot lost child settlement: %+v", got)
		}
		if goal := owner.goalSnapshot(); goal.TokensUsed != 10 || goal.ModelCalls != 1 {
			t.Fatalf("snapshot lost goal charge: %+v", goal)
		}
	}
}

func TestMemoryAccountingAndOutboxNeverReadSameNamedDiskChild(t *testing.T) {
	a, _ := sessionRepairAgent(t, true)
	goal := journalGoal(t, a)
	edge := recoveryChildEdge(a.SessionID(), goal.ID, a.SessionID(), "disk-only-child")
	if _, err := a.persistenceHandle().CommitEvents("queued-child", edge); err != nil {
		t.Fatal(err)
	}
	cfg := *a.services.cfg
	cfg.NoSessionPersistence = false
	child, err := openSessionPersistence(&cfg, "disk-only-child", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	commitMemoryAccountingRequest(t, child, a.SessionID(), goal.ID, "disk-only-child", "unrelated-disk-request", 20, 10, true)
	outbox := edge
	outbox.Child.Run.Status = "succeeded"
	outbox.Child.Run.Output = "from unrelated disk storage"
	outbox.DeliveryID = "disk-delivery"
	if _, err := child.CommitEvents("disk-outbox", outbox); err != nil {
		t.Fatal(err)
	}
	reopened, err := a.OpenSession(a.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.Close)
	run, err := reopened.services.supervisor.lookup(edge.Child.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	run.mu.Lock()
	status, output := run.fact.Child.Run.Status, run.fact.Child.Run.Output
	run.mu.Unlock()
	if status != "unknown" || output != "" || reopened.Usage().TurnCount != 0 || reopened.goalSnapshot().TokensUsed != 0 {
		t.Fatalf("memory recovery imported disk state: status=%s output=%q usage=%+v goal=%+v", status, output, reopened.Usage(), reopened.goalSnapshot())
	}
}
