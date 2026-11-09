package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/llm"
)

func TestChildSlotsResizeKeepsActiveReservations(t *testing.T) {
	slots := newChildSlots(2)
	if err := slots.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := slots.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	slots.setLimit(1)
	slots.release()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := slots.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shrink admitted waiter: %v", err)
	}
	slots.release()
	if err := slots.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	acquired := make(chan error, 1)
	go func() { acquired <- slots.acquire(context.Background()) }()
	slots.setLimit(2)
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("growth did not wake queued child")
	}
	slots.release()
	slots.release()
	slots.mu.Lock()
	active := slots.active
	slots.mu.Unlock()
	if active != 0 {
		t.Fatalf("leaked reservations=%d", active)
	}
}

func TestGuardianSlotsIndependentFromTaskAndToolLimits(t *testing.T) {
	cfg := config.Default()
	cfg.MaxParallelTools = 1
	cfg.MaxConcurrentAgents = 2
	a := &Agent{}
	a.services.cfg = &cfg
	task := slotsForParent(a, childPurposeTask)
	if task != slotsForParent(a, childPurposeReview) {
		t.Fatal("review must share task quota")
	}
	for i := 0; i < 2; i++ {
		if err := task.acquire(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	guardian := slotsForParent(a, childPurposeGuardian)
	if guardian == task {
		t.Fatal("guardian shares task quota")
	}
	if err := guardian.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg.MaxConcurrentAgents = 1
	task.setLimit(cfg.MaxConcurrentAgents)
	if slotsForParent(a, childPurposeTask) != task || slotsForParent(a, childPurposeGuardian) != guardian {
		t.Fatal("limiter identity changed")
	}
	guardian.release()
	task.release()
	task.release()
}

func TestChildTaskDefaults(t *testing.T) {
	for _, tc := range []struct{ role, workspace string }{{"worker", "isolated"}, {"explorer", "shared"}} {
		task, err := resolveChildTask(childTask{Role: tc.role})
		if err != nil || task.WorkspaceMode != tc.workspace || task.ContextMode != "fresh" {
			t.Fatalf("%+v %v", task, err)
		}
	}
}

func TestReloadResizesExistingChildLimiter(t *testing.T) {
	provider := &childRuntimeTestProvider{name: "resize-reload", stream: func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
		return llm.StreamResult{Text: "ok", FinishReason: "stop"}, nil
	}}
	a := newJournalRuntimeAgent(t, provider)
	task := slotsForParent(a, childPurposeTask)
	guardian := slotsForParent(a, childPurposeGuardian)
	if err := task.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer task.release()
	if err := os.MkdirAll(filepath.Join(a.services.cfg.Workspace, ".ccdp"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.services.cfg.Workspace, ".ccdp", "settings.json"), []byte(`{"max_parallel_tools":2,"max_concurrent_agents":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	changeSettingsForTest(t, a, "")
	if task != slotsForParent(a, childPurposeTask) || guardian != slotsForParent(a, childPurposeGuardian) {
		t.Fatal("reload replaced an active limiter")
	}
	cfg := a.configSnapshot()
	task.mu.Lock()
	limit, active := task.max, task.active
	task.mu.Unlock()
	if cfg.MaxParallelTools != 2 || cfg.MaxConcurrentAgents != 1 || limit != 1 || active != 1 {
		t.Fatalf("reload lost limit or reservation: tools=%d agents=%d limit=%d active=%d", cfg.MaxParallelTools, cfg.MaxConcurrentAgents, limit, active)
	}
}
