package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ccdp/internal/messages"
	"ccdp/internal/session"
	"ccdp/internal/session/archive"
)

func todoJournalCall(priority string) messages.ToolCall {
	return messages.ToolCall{Name: "TodoWrite", Arguments: map[string]any{"todos": []any{map[string]any{"content": "retain task", "status": "pending", "priority": priority}}}}
}

func TestTodoJournalRestoresWithoutSidecar(t *testing.T) {
	for _, sidecar := range []string{"missing", "stale", "corrupt"} {
		t.Run(sidecar, func(t *testing.T) {
			a := newRuntimeAgent(t)
			if out, failed := executeTestTool(t, a, todoJournalCall("high")); failed {
				t.Fatal(out)
			}
			// Priority-only edits must also be recorded.
			if out, failed := executeTestTool(t, a, todoJournalCall("low")); failed {
				t.Fatal(out)
			}
			cfg, id := a.configSnapshot(), a.SessionID()
			path := filepath.Join(a.services.resources.SessionDir(), "todos.json")
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("task tool still writes a sidecar: %v", err)
			}
			a.Close()
			if sidecar != "missing" {
				data := []byte(`{"1":{"ID":"1","Content":"stale task","Status":"completed","Priority":"high"}}`)
				if sidecar == "corrupt" {
					data = []byte(`{"1":`)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			snapshot, err := archive.Load(cfg.SessionDir, id)
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Tasks) != 1 || snapshot.Tasks[0].Priority != "low" {
				t.Fatalf("journal lost priority: %+v", snapshot.Tasks)
			}
			resumed, err := Resume(&cfg, snapshot)
			if err != nil {
				t.Fatal(err)
			}
			defer resumed.Close()
			if got := resumed.todoSection(); !strings.Contains(got, "[ ] (low) retain task") || strings.Contains(got, "stale task") {
				t.Fatalf("resumed prompt=%q", got)
			}
			resumed.mu.Lock()
			view := resumed.tasksViewLocked()
			resumed.mu.Unlock()
			if len(view) != 1 || view[0].Title != "retain task" || view[0].Status != "pending" {
				t.Fatalf("resumed view=%+v", view)
			}
		})
	}
}

func TestTodoJournalFailureDoesNotPublish(t *testing.T) {
	a := newRuntimeAgent(t)
	if out, failed := executeTestTool(t, a, todoJournalCall("low")); failed {
		t.Fatal(out)
	}
	backend := journalBackend(t, a.persistenceHandle())
	backend.mu.Lock()
	backend.store = requestJournalFailEventStore{Store: backend.store, event: session.EventTypeTasksChanged}
	backend.mu.Unlock()
	out, failed := executeTestTool(t, a, todoJournalCall("high"))
	if !failed {
		t.Fatalf("injected persistence failure accepted: %s", out)
	}
	if got := a.todoSection(); !strings.Contains(got, "(low) retain task") || strings.Contains(got, "(high)") {
		t.Fatalf("uncommitted task became visible: %q", got)
	}
	a.mu.Lock()
	tasks := append([]session.Task(nil), a.conversation.durable.Tasks...)
	a.mu.Unlock()
	cfg, id := a.configSnapshot(), a.SessionID()
	a.Close()
	saved, err := archive.Load(cfg.SessionDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tasks, saved.Tasks) {
		t.Fatalf("live and durable tasks differ: %+v / %+v", tasks, saved.Tasks)
	}
}

func TestTodoJournalOrderAndSessionIsolation(t *testing.T) {
	a, b := newRuntimeAgent(t), newRuntimeAgent(t)
	var items []any
	for i := 1; i <= 12; i++ {
		items = append(items, map[string]any{"content": fmt.Sprintf("step-%02d", i)})
	}
	if out, failed := executeTestTool(t, a, messages.ToolCall{Name: "TodoWrite", Arguments: map[string]any{"todos": items}}); failed {
		t.Fatal(out)
	}
	if got := b.todoSection(); got != "" {
		t.Fatalf("another session received tasks: %q", got)
	}
	section := a.todoSection()
	if strings.Index(section, "step-02") > strings.Index(section, "step-10") {
		t.Fatalf("task order changed: %s", section)
	}
	if out, failed := executeTestTool(t, a, messages.ToolCall{Name: "TodoWrite", Arguments: map[string]any{"todos": []any{}}}); failed {
		t.Fatal(out)
	}
	if got := a.todoSection(); got != "" {
		t.Fatalf("empty list did not clear tasks: %q", got)
	}
}

func TestTodoMemoryResumeOldPriority(t *testing.T) {
	cfg := testPersistenceConfig(t)
	cfg.NoSessionPersistence = true
	snapshot := &archive.Snapshot{ID: "todo-memory-resume", Workspace: cfg.Workspace, Model: cfg.Model, Tasks: []session.Task{{TaskID: "1", Title: "old record", Status: "pending"}}}
	a, err := Resume(&cfg, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if got := a.todoSection(); !strings.Contains(got, "(medium) old record") {
		t.Fatalf("legacy tasks lost: %q", got)
	}
}
