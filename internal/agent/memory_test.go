package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"ccdp/internal/config"
	"ccdp/internal/projectmemory"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/tools"
)

func newMemoryAgent(t *testing.T) *Agent {
	t.Helper()
	cfg := config.Default()
	cfg.Workspace = t.TempDir()
	cfg.SessionDir = t.TempDir()
	cfg.ProjectMemoryDir = t.TempDir()
	cfg.EnableProjectMemory = true
	a, err := testNew(t, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestProjectMemoryToolsStoreExplicitNotes(t *testing.T) {
	a := newMemoryAgent(t)
	defer a.Close()
	ctx := &tools.Context{Context: context.Background()}
	a.bindProjectMemoryInvocation(ctx, a.configSnapshot(), "turn-1")
	tool := &updateProjectMemoryTool{}
	args := map[string]any{"action": "save", "id": "tests", "title": "Tests", "summary": "Unit test command", "body": "go test ./internal/..."}
	result, err := tool.Execute(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	var entry projectmemory.Entry
	if err = json.Unmarshal(result.Data, &entry); err != nil {
		t.Fatal(err)
	}
	if entry.SourceSession != a.SessionID() {
		t.Fatalf("%+v", entry)
	}
	args["body"] = "different content"
	if _, err = tool.Execute(ctx, args); !errors.Is(err, projectmemory.ErrConflict) {
		t.Fatalf("stale update: %v", err)
	}
	index, err := projectMemorySection(a.configSnapshot())
	if err != nil || !strings.Contains(index, "tests") {
		t.Fatalf("%q %v", index, err)
	}
	read, err := (&readProjectMemoryTool{}).Execute(ctx, map[string]any{"id": "tests"})
	if err != nil || !strings.Contains(read.Text, "go test ./internal") {
		t.Fatalf("%+v %v", read, err)
	}
}

func TestProjectMemoryDisabledDoesNotTouchDisk(t *testing.T) {
	cfg := config.Default()
	cfg.EnableProjectMemory = false
	cfg.Workspace = t.TempDir()
	cfg.ProjectMemoryDir = t.TempDir() + "/uncreated"
	text, err := projectMemorySection(cfg)
	if err != nil || text != "" {
		t.Fatalf("%q %v", text, err)
	}
	if _, err = os.Stat(cfg.ProjectMemoryDir); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestProjectMemoryNoPersistenceIsReadOnly(t *testing.T) {
	a := newMemoryAgent(t)
	defer a.Close()
	a.mu.Lock()
	a.services.cfg.NoSessionPersistence = true
	a.mu.Unlock()
	ctx := &tools.Context{Context: context.Background()}
	a.bindProjectMemoryInvocation(ctx, a.configSnapshot(), "turn-1")
	_, err := (&updateProjectMemoryTool{}).Execute(ctx, map[string]any{"action": "save", "id": "x", "title": "X", "body": "body"})
	if !errors.Is(err, projectmemory.ErrReadOnly) {
		t.Fatal(err)
	}
}

func TestProjectMemoryFrozenWithStep(t *testing.T) {
	a := newMemoryAgent(t)
	defer a.Close()
	store, err := projectMemoryStore(a.configSnapshot(), false)
	if err != nil {
		t.Fatal(err)
	}
	e, err := store.Update(context.Background(), projectmemory.Entry{ID: "test", Title: "Old title", Summary: "Old summary", Body: "details"}, "")
	if err != nil {
		t.Fatal(err)
	}
	step, err := a.beginStepChecked()
	if err != nil {
		t.Fatal(err)
	}
	defer a.releaseStepContext(step)
	e.Title = "New title"
	if _, err = store.Update(context.Background(), e, e.Body); err != nil {
		t.Fatal(err)
	}
	text := joinSystemBlocks(step.systemBlocks)
	if !strings.Contains(text, "Old title") || strings.Contains(text, "New title") {
		t.Fatalf("step memory changed: %s", text)
	}
}

func TestProjectMemoryToggleUsesDurableSettings(t *testing.T) {
	a := newMemoryAgent(t)
	defer a.Close()
	receipt, err := a.Submit(context.Background(), protocol.Command{
		Type:          protocol.CommandProjectMemory,
		ProjectMemory: &protocol.ProjectMemoryCommand{Action: "disable"},
	})
	if err != nil || receipt.Status != protocol.ReceiptApplied {
		t.Fatalf("%+v %v", receipt, err)
	}
	if a.configSnapshot().EnableProjectMemory {
		t.Fatal("disable was not applied")
	}
	records, err := a.persistenceHandle().Read(session.Beginning)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, record := range records {
		if event, ok := record.Event.(*session.SettingsChanged); ok {
			seen = true
			if event.Settings.ProjectMemoryEnabled {
				continue
			}
			return
		}
	}
	t.Fatalf("disabled setting not saved (settings seen=%v)", seen)
}

func TestProjectMemorySettingSurvivesResumeAndReload(t *testing.T) {
	a := newMemoryAgent(t)
	cfg := a.configSnapshot()
	id := a.SessionID()
	a.Close()
	snapshot, err := LoadSession(cfg.SessionDir, id)
	if err != nil {
		t.Fatal(err)
	}
	cfg.EnableProjectMemory = false
	resumed, err := Resume(&cfg, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if !resumed.configSnapshot().EnableProjectMemory {
		t.Fatal("saved memory setting not restored")
	}
	cmd := protocol.Command{ID: "reload-memory", Type: protocol.CommandReloadSettings,
		Reload: &protocol.ReloadCommand{}}
	receipt, err := resumed.Submit(context.Background(), cmd)
	if err != nil || receipt.Status != protocol.ReceiptScheduled {
		t.Fatalf("%+v %v", receipt, err)
	}
	if done := waitWorkflowCompleted(t, resumed, cmd.ID); done.Outcome != "success" {
		t.Fatalf("%+v", done)
	}
	if !resumed.configSnapshot().EnableProjectMemory {
		t.Fatal("reload discarded the restored session's memory setting")
	}
}

func TestProjectMemoryCommandsRecordSourceAndDelete(t *testing.T) {
	a := newMemoryAgent(t)
	defer a.Close()
	save := protocol.Command{ID: "memory-save", Type: protocol.CommandProjectMemory,
		ProjectMemory: &protocol.ProjectMemoryCommand{Action: "save", ID: "build", Title: "Build", Body: "go test ./..."}}
	receipt, err := a.Submit(context.Background(), save)
	if err != nil || receipt.Status != protocol.ReceiptScheduled {
		t.Fatalf("%+v %v", receipt, err)
	}
	if done := waitWorkflowCompleted(t, a, save.ID); done.Outcome != "success" {
		t.Fatalf("%+v", done)
	}
	store, err := projectMemoryStore(a.configSnapshot(), true)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := store.Read("build")
	if err != nil || entry.SourceCommand != string(save.ID) || entry.SourceTurn != "" {
		t.Fatalf("%+v %v", entry, err)
	}
	remove := protocol.Command{ID: "memory-delete", Type: protocol.CommandProjectMemory,
		ProjectMemory: &protocol.ProjectMemoryCommand{Action: "delete", ID: "build"}}

	receipt, err = a.Submit(context.Background(), remove)
	if err != nil || receipt.Status != protocol.ReceiptScheduled {
		t.Fatalf("%+v %v", receipt, err)
	}
	if done := waitWorkflowCompleted(t, a, remove.ID); done.Outcome != "success" {
		t.Fatalf("%+v", done)
	}
	if _, err = store.Read("build"); !os.IsNotExist(err) {
		t.Fatalf("note still exists: %v", err)
	}
}

func TestProjectMemoryToolUsesInvocationWorkspace(t *testing.T) {
	a := newMemoryAgent(t)
	defer a.Close()
	cfg := a.configSnapshot()
	ctx := &tools.Context{Context: context.Background()}
	a.bindProjectMemoryInvocation(ctx, cfg, "turn-original")
	a.mu.Lock()
	a.services.cfg.Workspace = t.TempDir()
	a.mu.Unlock()
	_, err := (&updateProjectMemoryTool{}).Execute(ctx, map[string]any{
		"action": "save", "id": "frozen", "title": "Frozen", "body": "original project",
	})
	if err != nil {
		t.Fatal(err)
	}
	original, _ := projectMemoryStore(cfg, true)
	entry, err := original.Read("frozen")
	if err != nil || entry.SourceTurn != "turn-original" {
		t.Fatalf("%+v %v", entry, err)
	}
	current, _ := projectMemoryStore(a.configSnapshot(), true)
	if entries, err := current.List(); err != nil || len(entries) != 0 {
		t.Fatalf("wrote to new workspace: %v %v", entries, err)
	}
}
