package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccdp/internal/sandbox"
)

func TestTodoPersistsAcrossStores(t *testing.T) {
	dir := t.TempDir()
	resources := NewResources("todo-test", dir)
	t.Cleanup(func() { _ = resources.Close() })
	ctx := resources.Context(nil, dir, sandbox.New(dir))
	ctx.Args = map[string]any{
		"todos": []any{
			map[string]any{"content": "step one", "status": "in_progress", "priority": "high"},
			map[string]any{"content": "step two"},
		},
	}

	tool := NewTodoWriteTool()
	if _, err := tool.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Persisted to disk.
	data, err := os.ReadFile(filepath.Join(dir, "todos.json"))
	if err != nil || !strings.Contains(string(data), "step one") {
		t.Fatalf("todos not persisted: %v %s", err, data)
	}

	// A fresh store (simulating a restart) reads it back.
	restarted := NewTodoStore("todo-restart", dir)
	t.Cleanup(restarted.Close)
	sec := restarted.Section()
	if !strings.Contains(sec, "step one") || !strings.Contains(sec, "# Task list") {
		t.Errorf("section missing todos: %q", sec)
	}

	// Different session dirs are isolated.
	other := NewTodoStore("todo-other", t.TempDir())
	t.Cleanup(other.Close)
	if section := other.Section(); section != "" {
		t.Errorf("unrelated session should have no todos, got %q", section)
	}
}
