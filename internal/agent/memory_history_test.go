package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/llm"
	"ccdp/internal/permissions"
	"ccdp/internal/plugin"
	"ccdp/internal/projectmemory"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func newMemoryProviderAgent(t *testing.T, p llm.Provider, history bool) *Agent {
	t.Helper()
	cfg := config.Default()
	cfg.Workspace, cfg.SessionDir, cfg.ProjectMemoryDir = t.TempDir(), t.TempDir(), t.TempDir()
	cfg.MemoryHistory, cfg.Model, cfg.PermissionMode = history, "memory-test", string(permissions.ModeBypass)
	cfg.MaxTurns = 4
	registry := plugin.NewModelRegistry()
	registry.Register(p)
	registry.Route(cfg.Model, p.Name())
	a, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true, ProviderRegistry: registry})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

func TestMemoryAutomaticToolWriteVisibleOnNextRequest(t *testing.T) {
	var calls atomic.Int32
	p := &childRuntimeTestProvider{name: "memory-tool", stream: func(_ context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		n := calls.Add(1)
		text := fmt.Sprint(req.Messages)
		if !strings.Contains(text, "Proactively save") {
			return llm.StreamResult{}, fmt.Errorf("automatic memory instructions missing")
		}
		if n == 1 {
			return llm.StreamResult{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{ID: "remember", Type: "function", Function: llm.Function{Name: "UpdateProjectMemory", Arguments: `{"action":"save","id":"testing","kind":"feedback","body":"Run integration tests locally before release."}`}}}}, nil
		}
		if !strings.Contains(text, "Run integration tests locally before release.") {
			return llm.StreamResult{}, fmt.Errorf("saved memory not visible in next request")
		}
		return llm.StreamResult{Text: "Remembered.", FinishReason: "stop"}, nil
	}}
	a := newMemoryProviderAgent(t, p, false)
	submitChildRuntimeTestInput(t, a, "remember-input", "Remember that integration tests run locally before release.")
	view := waitChildRuntimeTestIdle(t, a)
	if view.LastTurn.Status != protocol.TurnSucceeded || calls.Load() != 2 {
		t.Fatalf("turn: %+v, calls=%d", view.LastTurn, calls.Load())
	}
	store, _ := projectMemoryStore(a.configSnapshot(), true)
	if entry, err := store.Read("testing"); err != nil || entry.SourceTurn != "turn-1" {
		t.Fatalf("not persisted: %+v %v", entry, err)
	}
}

func TestMemoryHistoryExtractionUsesJournalAndDoesNotResurrect(t *testing.T) {
	var extractions atomic.Int32
	p := &childRuntimeTestProvider{name: "memory-history", stream: func(_ context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		if strings.Contains(fmt.Sprint(req.Messages), "Extract only durable project memories") {
			extractions.Add(1)
			return llm.StreamResult{FinishReason: "stop", Text: `{"notes":[{"turn_id":"turn-1","title":"Testing","kind":"feedback","body":"Integration tests require the local Redis instance."}]}`}, nil
		}
		return llm.StreamResult{Text: "Understood.", FinishReason: "stop"}, nil
	}}
	a := newMemoryProviderAgent(t, p, true)
	submitChildRuntimeTestInput(t, a, "history-input", "Remember for this project: integration tests require a local Redis instance.")
	waitChildRuntimeTestIdle(t, a)
	if extractions.Load() != 0 {
		t.Fatal("ordinary turn ran a mandatory extraction")
	}
	output, err := a.extractMemoryHistory(context.Background(), a.configSnapshot(), "")
	if err != nil || !strings.Contains(output, "Remembered 1") {
		t.Fatalf("extract: %s %v", output, err)
	}
	store, _ := projectMemoryStore(a.configSnapshot(), false)
	entries, err := store.List()
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries: %+v %v", entries, err)
	}
	if err := store.Delete(context.Background(), entries[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.extractMemoryHistory(context.Background(), a.configSnapshot(), ""); err != nil {
		t.Fatal(err)
	}
	if extractions.Load() != 1 {
		t.Fatal("processed history was sent again")
	}
	if _, err := store.Read(entries[0].ID); !os.IsNotExist(err) {
		t.Fatal("deleted note reappeared")
	}
}

func TestMemoryOffCancelsBackgroundExtraction(t *testing.T) {
	started := make(chan struct{})
	p := &childRuntimeTestProvider{name: "memory-cancel", stream: func(ctx context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		if strings.Contains(fmt.Sprint(req.Messages), "Extract only durable project memories") {
			close(started)
			<-ctx.Done()
			return llm.StreamResult{}, ctx.Err()
		}
		return llm.StreamResult{Text: "Understood.", FinishReason: "stop"}, nil
	}}
	a := newMemoryProviderAgent(t, p, true)
	submitChildRuntimeTestInput(t, a, "before-off", "This project uses pnpm.")
	waitChildRuntimeTestIdle(t, a)
	cmd := protocol.Command{ID: "extract-background", Type: protocol.CommandProjectMemory, ProjectMemory: &protocol.ProjectMemoryCommand{Action: "extract-auto"}}
	if r, err := a.Submit(context.Background(), cmd); err != nil || r.Rejected() {
		t.Fatalf("%+v %v", r, err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("extractor did not start")
	}
	if r, err := a.Submit(context.Background(), protocol.Command{Type: protocol.CommandProjectMemory, ProjectMemory: &protocol.ProjectMemoryCommand{Action: "disable"}}); err != nil || r.Rejected() {
		t.Fatalf("disable: %+v %v", r, err)
	}
	if done := waitWorkflowCompleted(t, a, cmd.ID); done.Outcome == "success" {
		t.Fatal("cancelled extraction succeeded")
	}
}

func TestMemoryHistoryEligibilityAndSteeredCorrections(t *testing.T) {
	var rows []session.Record
	add := func(e session.Event) { rows = append(rows, session.Record{Seq: uint64(len(rows) + 1), Event: e}) }
	settings := session.Settings{Workspace: "/project", ProjectMemoryEnabled: false, MemoryHistory: true}
	add(&session.SettingsChanged{Settings: settings})
	for n := 1; n <= 3; n++ {
		id := fmt.Sprintf("turn-%d", n)
		add(&session.InputQueued{InputID: id, Text: "old preference"})
		add(&session.InputDelivered{InputID: id, TurnID: id})
		add(&session.TurnStarted{TurnID: id})
		if n == 2 {
			add(&session.InputQueued{InputID: "steer", Text: "Correction: use the new preference."})
			add(&session.InputDelivered{InputID: "steer", TurnID: id})
		}
		if n < 3 {
			add(&session.TurnFinished{TurnID: id, Outcome: "success"})
		}
		settings.ProjectMemoryEnabled = true
		add(&session.SettingsChanged{Settings: settings})
	}
	turns, _ := memoryHistoryTurns(rows, 0, true, func(w string) bool { return w == "/project" })
	if len(turns) != 1 || turns[0].ID != "turn-2" || !strings.Contains(turns[0].Text, "Correction:") {
		t.Fatalf("off/incomplete turns included or steer lost: %+v", turns)
	}
	add(&session.ConversationRewound{})
	turns, _ = memoryHistoryTurns(rows, 0, true, func(string) bool { return true })
	if len(turns) != 0 {
		t.Fatal("rewound history extracted")
	}
}

func TestMemoryCorruptionDoesNotBlockPreparation(t *testing.T) {
	a := newMemoryAgent(t)
	defer a.Close()
	store, _ := projectMemoryStore(a.configSnapshot(), false)
	if err := os.MkdirAll(store.Directory(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Directory()+"/bad.md", []byte("---\ninvalid\n---\nbody"), 0600); err != nil {
		t.Fatal(err)
	}
	step, err := a.beginStepChecked()
	if err != nil {
		t.Fatalf("memory failure blocked chat: %v", err)
	}
	defer a.releaseStepContext(step)
	if !strings.Contains(joinSystemBlocks(step.systemBlocks), "memory unavailable") {
		t.Fatal("memory failure hidden")
	}
}

func TestMemoryHistoryFillsGapsAndMergesFullNotes(t *testing.T) {
	var extractionCalls atomic.Int32
	original := strings.Repeat("Keep this verified detail. ", 8) + "Integration tests run locally."
	merged := original + " Start Redis first."
	p := &childRuntimeTestProvider{name: "memory-merge", stream: func(_ context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		if !strings.Contains(fmt.Sprint(req.Messages), "Extract only durable project memories") {
			return llm.StreamResult{Text: "Understood.", FinishReason: "stop"}, nil
		}
		extractionCalls.Add(1)
		var input struct {
			Existing []memoryHistoryNote `json:"existing_memory"`
			Turns    []memoryHistoryTurn `json:"turns"`
		}
		if err := json.Unmarshal([]byte(req.Messages[1].Content.(string)), &input); err != nil {
			return llm.StreamResult{}, err
		}
		if len(input.Turns) != 1 || input.Turns[0].ID != "turn-1" {
			return llm.StreamResult{}, fmt.Errorf("partially remembered turn missing: %+v", input.Turns)
		}
		foundProject, foundUser := false, false
		for _, e := range input.Existing {
			if e.ID == "testing" && e.Scope == "project" && e.Body == original {
				foundProject = true
			}
			if e.ID == "language" && e.Scope == "user" && e.Body == "Prefer Chinese replies." {
				foundUser = true
			}
		}
		if !foundProject || !foundUser {
			return llm.StreamResult{}, fmt.Errorf("full scoped notes missing: %+v", input.Existing)
		}
		if input.Turns[0].Outcome != "success" {
			return llm.StreamResult{}, fmt.Errorf("missing turn outcome")
		}
		raw, _ := json.Marshal(struct {
			Notes []projectmemory.HistoryNote `json:"notes"`
		}{[]projectmemory.HistoryNote{
			{ID: "testing", Turn: "turn-1", Title: "Testing", Kind: "feedback", Body: merged},
			{Turn: "turn-1", Title: "Release", Kind: "reference", Body: "Release checklist lives in the team wiki."},
		}})
		return llm.StreamResult{Text: string(raw), FinishReason: "stop"}, nil
	}}
	a := newMemoryProviderAgent(t, p, false)
	cfg := a.configSnapshot()
	store, _ := projectMemoryStore(cfg, false)
	user, _ := scopedMemoryStore(cfg, "user", false)
	ctx := context.Background()
	if _, err := store.Update(ctx, projectmemory.Entry{ID: "testing", Kind: "feedback", Body: original, SourceSession: a.SessionID(), SourceTurn: "turn-1"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := user.Update(ctx, projectmemory.Entry{ID: "language", Kind: "user", Body: "Prefer Chinese replies.", SourceSession: a.SessionID(), SourceTurn: "turn-1"}, ""); err != nil {
		t.Fatal(err)
	}
	submitChildRuntimeTestInput(t, a, "merge-history", "Start Redis before integration tests. The release checklist is in the team wiki.")
	waitChildRuntimeTestIdle(t, a)
	report, err := a.extractMemoryHistory(ctx, cfg, "")
	if err != nil || !strings.Contains(report, "Remembered 2") {
		t.Fatalf("extract: %s %v", report, err)
	}
	entries, _ := store.List()
	updated, _ := store.Read("testing")
	if len(entries) != 2 || updated.Body != merged {
		t.Fatalf("merge lost details or duplicated: %+v", entries)
	}
	if _, err := a.extractMemoryHistory(ctx, cfg, ""); err != nil {
		t.Fatal(err)
	}
	if extractionCalls.Load() != 1 {
		t.Fatal("cursor did not prevent repeat extraction")
	}
}

func TestMemoryHistoryConcurrentScopeChangesKeepCursor(t *testing.T) {
	for _, scope := range []string{"project", "user"} {
		t.Run(scope, func(t *testing.T) {
			var store *projectmemory.Store
			p := &childRuntimeTestProvider{name: "memory-concurrent", stream: func(_ context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
				if strings.Contains(fmt.Sprint(req.Messages), "Extract only durable project memories") {
					// Simulate another session correcting memory while the model is running.
					if _, err := store.Edit(context.Background(), "tool", "pnpm", "bun", projectmemory.Entry{SourceSession: "other", SourceTurn: "turn-2"}); err != nil {
						return llm.StreamResult{}, err
					}
					return llm.StreamResult{FinishReason: "stop", Text: `{"notes":[{"turn_id":"turn-1","kind":"project","body":"Use pnpm."}]}`}, nil
				}
				return llm.StreamResult{Text: "Understood.", FinishReason: "stop"}, nil
			}}
			a := newMemoryProviderAgent(t, p, false)
			cfg := a.configSnapshot()
			store, _ = scopedMemoryStore(cfg, scope, false)
			if _, err := store.Update(context.Background(), projectmemory.Entry{ID: "tool", Body: "Use pnpm."}, ""); err != nil {
				t.Fatal(err)
			}
			submitChildRuntimeTestInput(t, a, "concurrent-memory", "Use pnpm for this project.")
			waitChildRuntimeTestIdle(t, a)
			if _, err := a.extractMemoryHistory(context.Background(), cfg, ""); !errors.Is(err, projectmemory.ErrConflict) {
				t.Fatalf("stale extraction: %v", err)
			}
			entry, _ := store.Read("tool")
			if entry.Body != "Use bun." {
				t.Fatalf("correction overwritten: %+v", entry)
			}
			project, _ := projectMemoryStore(cfg, false)
			job, err := project.BeginHistory(context.Background(), a.SessionID())
			if err != nil {
				t.Fatal(err)
			}
			defer job.Close()
			if job.Cursor != 0 {
				t.Fatal("conflict advanced extraction cursor")
			}
		})
	}
}

func TestMemoryHistoryDeletionExcludesSourceAcrossModes(t *testing.T) {
	var calls atomic.Int32
	p := &childRuntimeTestProvider{name: "memory-forget-modes", stream: func(_ context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		if strings.Contains(fmt.Sprint(req.Messages), "Extract only durable project memories") {
			calls.Add(1)
			return llm.StreamResult{FinishReason: "stop", Text: `{"notes":[{"turn_id":"turn-1","kind":"feedback","body":"Use local Redis."}]}`}, nil
		}
		return llm.StreamResult{Text: "Understood.", FinishReason: "stop"}, nil
	}}
	a := newMemoryProviderAgent(t, p, true)
	cfg := a.configSnapshot()
	submitChildRuntimeTestInput(t, a, "forget-modes", "Use local Redis.")
	waitChildRuntimeTestIdle(t, a)
	if _, err := a.extractMemoryHistory(context.Background(), cfg, ""); err != nil {
		t.Fatal(err)
	}
	store, _ := projectMemoryStore(cfg, false)
	entries, _ := store.List()
	if err := store.Delete(context.Background(), entries[0].ID); err != nil {
		t.Fatal(err)
	}
	// The automatic cursor has never run. Tombstones still exclude the source.
	if _, err := a.extractMemoryHistoryMode(context.Background(), cfg, "", true); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("forgotten source was sent to the model again")
	}
}

func TestMemoryHistoryOversizedSnapshotDoesNotSpendOrAdvance(t *testing.T) {
	var extractions atomic.Int32
	p := &childRuntimeTestProvider{name: "memory-budget", stream: func(_ context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		if strings.Contains(fmt.Sprint(req.Messages), "Extract only durable project memories") {
			extractions.Add(1)
		}
		return llm.StreamResult{Text: "Understood.", FinishReason: "stop"}, nil
	}}
	a := newMemoryProviderAgent(t, p, false)
	cfg := a.configSnapshot()
	store, _ := projectMemoryStore(cfg, false)
	for i := 0; i < 44; i++ {
		if _, err := store.Update(context.Background(), projectmemory.Entry{Body: fmt.Sprintf("Topic %d: %s", i, strings.Repeat("detail ", 450))}, ""); err != nil {
			t.Fatal(err)
		}
	}
	submitChildRuntimeTestInput(t, a, "memory-budget", "Use local Redis for this project.")
	waitChildRuntimeTestIdle(t, a)
	if _, err := a.extractMemoryHistory(context.Background(), cfg, ""); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized extraction: %v", err)
	}
	if extractions.Load() != 0 {
		t.Fatal("oversized input sent to model")
	}
	job, err := store.BeginHistory(context.Background(), a.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	defer job.Close()
	if job.Cursor != 0 {
		t.Fatal("oversized input consumed history")
	}
}
