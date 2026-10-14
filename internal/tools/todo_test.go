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
	sec, err := restarted.Section()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sec, "step one") || !strings.Contains(sec, "# Task list") {
		t.Errorf("section missing todos: %q", sec)
	}

	// Different session dirs are isolated.
	other := NewTodoStore("todo-other", t.TempDir())
	t.Cleanup(other.Close)
	if section, err := other.Section(); err != nil || section != "" {
		t.Errorf("unrelated session should have no todos, got %q", section)
	}
}

func TestTodoFailedPublicationPreservesMemory(t *testing.T) {
	root := t.TempDir()
	store := NewTodoStore("test", root)
	old := map[string]TodoState{"1": {ID: "1", Content: "original"}}
	if err := store.Store(old); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "todos.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := store.Store(map[string]TodoState{"1": {ID: "1", Content: "uncommitted"}}); err == nil {
		t.Fatal("write unexpectedly succeeded")
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got["1"].Content != "original" {
		t.Fatalf("unpublished state visible: %+v", got)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temporary publication files leaked: %v", entries)
	}
}

func TestTodoCorruptLoadNeverOverwritesRecord(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "todos.json")
	bad := []byte(`{"1":`)
	if err := os.WriteFile(path, bad, 0600); err != nil {
		t.Fatal(err)
	}
	store := NewTodoStore("test", root)
	if _, err := store.Load(); err == nil {
		t.Fatal("corrupt state loaded")
	}
	if _, err := store.Section(); err == nil {
		t.Fatal("corrupt state hidden in section")
	}
	if _, err := store.Snapshot(); err == nil {
		t.Fatal("corrupt state hidden in snapshot")
	}
	if err := store.Store(map[string]TodoState{}); err == nil {
		t.Fatal("corrupt state overwritten")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(bad) {
		t.Fatalf("record changed: %q", data)
	}
}
