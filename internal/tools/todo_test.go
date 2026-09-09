package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTodoPersistsAcrossStores(t *testing.T) {
	dir := t.TempDir()
	ctx := &Context{SessionDir: dir, Args: map[string]any{
		"todos": []any{
			map[string]any{"content": "step one", "status": "in_progress", "priority": "high"},
			map[string]any{"content": "step two"},
		},
	}}

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
	sec := TodoSection(dir)
	if !strings.Contains(sec, "step one") || !strings.Contains(sec, "# Task list") {
		t.Errorf("section missing todos: %q", sec)
	}

	// Different session dirs are isolated.
	if other := TodoSection(t.TempDir()); other != "" {
		t.Errorf("unrelated session should have no todos, got %q", other)
	}
}
