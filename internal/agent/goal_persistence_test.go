package agent

import (
	"reflect"
	"testing"
	"time"

	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func persistedTestGoal() *protocol.Goal {
	now := time.Unix(1234, 0).UTC()
	return &protocol.Goal{ID: "goal-1", Objective: "finish work", PendingObjective: "finish updated work", Status: protocol.GoalActive, TokenBudget: 1000, TokensUsed: 80, ModelCalls: 3, RunModelCalls: 2, ActiveTimeSeconds: 12.5, CreatedAt: now, UpdatedAt: now, TokensEstimated: true}
}

func TestGoalPersistenceReplayAndClear(t *testing.T) {
	cfg := testPersistenceConfig(t)
	p, err := openSessionPersistence(&cfg, "goal-replay", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	goal := persistedTestGoal()
	if err := p.PersistGoal(goal); err != nil {
		t.Fatal(err)
	}
	cursor := p.CurrentCursor()
	if err := p.PersistGoal(goal); err != nil {
		t.Fatal(err)
	}
	if p.CurrentCursor() != cursor {
		t.Fatal("identical goal added event")
	}
	original := protocol.CloneGoal(goal)
	goal.Objective = "caller mutation"
	snapshot, err := SessionSnapshotProjection(p, "goal-replay")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot.Goal, original) {
		t.Fatalf("goal = %+v", snapshot.Goal)
	}
	snapshot.Goal.Objective = "snapshot mutation"
	snapshot, err = SessionSnapshotProjection(p, "goal-replay")
	if err != nil || !reflect.DeepEqual(snapshot.Goal, original) {
		t.Fatalf("snapshot alias: %+v, %v", snapshot, err)
	}
	if err := p.PersistGoal(nil); err != nil {
		t.Fatal(err)
	}
	snapshot, err = SessionSnapshotProjection(p, "goal-replay")
	if err != nil || snapshot.Goal != nil {
		t.Fatalf("clear = %+v, %v", snapshot.Goal, err)
	}
}

func TestGoalSnapshotResumePreservesAccounting(t *testing.T) {
	goal := persistedTestGoal()
	snapshot := SessionSnapshot{ID: "goal-resume", CreatedAt: goal.CreatedAt, UpdatedAt: goal.UpdatedAt, Goal: goal, GoalAccountingIDs: map[string]bool{"request-2": true, "request-1": true}}
	events, err := memoryResumeEvents(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	store := session.NewMemoryStore()
	defer store.Close()
	if _, err := store.Commit(session.Beginning, session.Batch{TransactionID: "resume", Events: events}); err != nil {
		t.Fatal(err)
	}
	got, err := SessionSnapshotProjection(store, snapshot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Goal, goal) || !reflect.DeepEqual(got.GoalAccountingIDs, snapshot.GoalAccountingIDs) {
		t.Fatalf("resume = %+v, %+v", got.Goal, got.GoalAccountingIDs)
	}
	next := protocol.CloneGoal(goal)
	next.ID = "next"
	if _, err := store.Commit(store.CurrentCursor(), session.Batch{TransactionID: "next-goal", Events: []session.Event{session.GoalChanged{Goal: next, AccountingID: "next-request"}}}); err != nil {
		t.Fatal(err)
	}
	got, err = SessionSnapshotProjection(store, snapshot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.GoalAccountingIDs, map[string]bool{"next-request": true}) {
		t.Fatalf("old accounting retained: %+v", got.GoalAccountingIDs)
	}
}

func TestGoalForkStartsPausedWithIndependentAccounting(t *testing.T) {
	cfg := testPersistenceConfig(t)
	a, err := testNew(t, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	goal := persistedTestGoal()
	if err := a.persistGoal(goal); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.conversation.durable.Goal = protocol.CloneGoal(goal)
	a.mu.Unlock()
	id, err := a.Fork(0)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := LoadSession(cfg.SessionDir, id)
	if err != nil {
		t.Fatal(err)
	}
	got := snapshot.Goal
	if got == nil || got.ID == goal.ID || got.Objective != goal.PendingObjective || got.PendingObjective != "" || got.Status != protocol.GoalPaused || got.TokenBudget != goal.TokenBudget {
		t.Fatalf("fork goal = %+v", got)
	}
	if got.TokensUsed != 0 || got.ModelCalls != 0 || got.RunModelCalls != 0 || got.ActiveTimeSeconds != 0 || got.TokensEstimated || len(snapshot.GoalAccountingIDs) != 0 {
		t.Fatalf("fork inherited accounting: %+v", got)
	}
	a.mu.Lock()
	sourceGoal := protocol.CloneGoal(a.conversation.durable.Goal)
	a.mu.Unlock()
	if !reflect.DeepEqual(sourceGoal, goal) {
		t.Fatalf("fork mutated source goal: %+v", sourceGoal)
	}
}

func TestGoalResumePausesAndAppliesPendingEdit(t *testing.T) {
	for _, memory := range []bool{false, true} {
		name := "disk"
		if memory {
			name = "memory"
		}
		t.Run(name, func(t *testing.T) {
			cfg := testPersistenceConfig(t)
			goal := persistedTestGoal()
			var snapshot *SessionSnapshot
			if memory {
				cfg.NoSessionPersistence = true
				snapshot = &SessionSnapshot{ID: "goal-resume-active", CreatedAt: goal.CreatedAt, UpdatedAt: goal.UpdatedAt, Goal: goal, GoalAccountingIDs: map[string]bool{"request-1": true}}
			} else {
				p, err := openSessionPersistence(&cfg, "goal-resume-active", goal.CreatedAt)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = p.Commit(session.Batch{TransactionID: "goal", Events: []session.Event{session.GoalChanged{Goal: goal, AccountingID: "request-1"}}}); err != nil {
					t.Fatal(err)
				}
				if err = p.Close(); err != nil {
					t.Fatal(err)
				}
				snapshot, err = LoadSession(cfg.SessionDir, "goal-resume-active")
				if err != nil {
					t.Fatal(err)
				}
			}
			a, err := Resume(&cfg, snapshot)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			a.mu.Lock()
			got := protocol.CloneGoal(a.conversation.durable.Goal)
			ids := cloneGoalAccountingIDs(a.conversation.durable.GoalAccountingIDs)
			busy := a.turn.busy
			a.mu.Unlock()
			if got == nil || got.Status != protocol.GoalPaused || got.Objective != goal.PendingObjective || got.PendingObjective != "" || got.TokensUsed != goal.TokensUsed || got.ModelCalls != goal.ModelCalls || !ids["request-1"] || busy {
				t.Fatalf("resume = %+v ids=%+v busy=%v", got, ids, busy)
			}
			persisted, err := a.persistenceHandle().snapshotFromOwnedStore(a.SessionID())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(persisted.Goal, got) {
				t.Fatalf("pause was not persisted: %+v vs %+v", persisted.Goal, got)
			}
			if snapshot.Goal.Status != protocol.GoalActive || snapshot.Goal.PendingObjective == "" {
				t.Fatal("resume mutated caller snapshot")
			}
		})
	}
}
