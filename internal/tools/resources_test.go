package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResourcesSessionIsolationAndClose(t *testing.T) {
	root := t.TempDir()
	aDir := filepath.Join(root, "a")
	bDir := filepath.Join(root, "b")
	if err := os.MkdirAll(aDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bDir, 0o755); err != nil {
		t.Fatal(err)
	}
	a := NewResources("session-a", filepath.Join(root, "sessions", "a"))
	b := NewResources("session-b", filepath.Join(root, "sessions", "b"))
	t.Cleanup(func() {
		_ = a.Close()
		_ = b.Close()
	})

	aCtx := a.Context(nil, aDir, nil)
	aCtx.Args = map[string]any{"todos": []any{map[string]any{"content": "only A", "status": "in_progress"}}}
	if _, err := NewTodoWriteTool().Run(aCtx); err != nil {
		t.Fatalf("A TodoWrite: %v", err)
	}
	bCtx := b.Context(nil, bDir, nil)
	if got := b.Todos.Section(); got != "" {
		t.Fatalf("B observed A's todo state: %q", got)
	}

	aFile := filepath.Join(aDir, "file.txt")
	if err := os.WriteFile(aFile, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	aCtx.Args = map[string]any{"file_path": aFile}
	if _, err := NewReadTool().Run(aCtx); err != nil {
		t.Fatalf("A Read: %v", err)
	}
	bCtx.Args = map[string]any{"file_path": aFile, "old_string": "original", "new_string": "B"}
	if _, err := NewEditTool().Run(bCtx); err == nil || !strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("B used A's freshness state: %v", err)
	}

	// The two managers intentionally allocate distinct scope-tagged handles.
	aCtx.Args = map[string]any{"command": "sleep 30"}
	startedA, err := NewProcessStartTool().Run(aCtx)
	if err != nil {
		t.Fatalf("A ProcessStart: %v", err)
	}
	aPID := extractPID(t, startedA)
	bCtx.Args = map[string]any{"command": "cat"}
	startedB, err := NewProcessStartTool().Run(bCtx)
	if err != nil {
		t.Fatalf("B ProcessStart: %v", err)
	}
	bPID := extractPID(t, startedB)
	if aPID == bPID {
		t.Fatalf("scope-tagged handles unexpectedly collided: A=%d B=%d", aPID, bPID)
	}
	bCtx.Args = map[string]any{"pid": aPID, "wait_ms": 0}
	if _, err := NewProcessOutputTool().Run(bCtx); err == nil || !strings.Contains(err.Error(), "another session") {
		t.Fatalf("B accepted A's process handle: %v", err)
	}
	// Closing A must not touch B. Write a marker to B after closing A and read it
	// back from B's still-live process.
	if err := a.Close(); err != nil {
		t.Fatalf("close A: %v", err)
	}
	if _, err := NewProcessOutputTool().Run(aCtx); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("closed A accepted a new process operation: %v", err)
	}
	bCtx.Args = map[string]any{"pid": bPID, "input": "marker-from-b\n"}
	if _, err := NewProcessWriteTool().Run(bCtx); err != nil {
		t.Fatalf("B process was killed by closing A: %v", err)
	}
	bCtx.Args = map[string]any{"pid": bPID, "wait_ms": 1000}
	output, err := NewProcessOutputTool().Run(bCtx)
	if err != nil || !strings.Contains(output, "marker-from-b") {
		t.Fatalf("B output after A close = %q, %v", output, err)
	}
	bCtx.Args = map[string]any{"pid": bPID}
	if _, err := NewProcessStopTool().Run(bCtx); err != nil {
		t.Fatalf("close B process: %v", err)
	}
}

func TestResourcesCloseIdempotent(t *testing.T) {
	r := NewResources("close-test", t.TempDir())
	first := r.Close()
	second := r.Close()
	if (first == nil) != (second == nil) {
		t.Fatalf("Close changed result: first=%v second=%v", first, second)
	}
	if err := r.CheckOpen(); err == nil {
		t.Fatal("closed resources still open")
	}
}
