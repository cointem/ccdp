package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func freshCtx(t *testing.T) *Context {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(ClearFileReadState)
	ClearFileReadState()
	return &Context{WorkingDir: dir, Args: map[string]any{}}
}

func runTool(t *testing.T, tool Tool, ctx *Context, args map[string]any) (string, error) {
	t.Helper()
	c := *ctx
	c.Args = args
	return tool.Run(&c)
}

func TestEditRequiresFreshRead(t *testing.T) {
	ctx := freshCtx(t)
	path := filepath.Join(ctx.WorkingDir, "a.txt")
	if err := os.WriteFile(path, []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Editing without a prior Read is rejected (read-before-edit gate).
	if _, err := runTool(t, NewEditTool(), ctx, map[string]any{
		"file_path": path, "old_string": "world", "new_string": "there",
	}); err == nil || !strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("expected not-read error, got %v", err)
	}

	// After a Read, the edit succeeds.
	if _, err := runTool(t, NewReadTool(), ctx, map[string]any{"file_path": path}); err != nil {
		t.Fatal(err)
	}
	out, err := runTool(t, NewEditTool(), ctx, map[string]any{
		"file_path": path, "old_string": "world", "new_string": "there",
	})
	if err != nil || !strings.Contains(out, "+ there") {
		t.Fatalf("edit after read failed: %q, %v", out, err)
	}
}

func TestEditDetectsExternalModification(t *testing.T) {
	ctx := freshCtx(t)
	path := filepath.Join(ctx.WorkingDir, "b.txt")
	if err := os.WriteFile(path, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(t, NewReadTool(), ctx, map[string]any{"file_path": path}); err != nil {
		t.Fatal(err)
	}

	// External modification after the Read (user, linter…).
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte("externally changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := runTool(t, NewEditTool(), ctx, map[string]any{
		"file_path": path, "old_string": "one", "new_string": "two",
	})
	if err == nil || !strings.Contains(err.Error(), "modified since") {
		t.Fatalf("expected stale-read rejection, got %v", err)
	}

	// Re-reading clears the staleness and the edit goes through.
	if _, err := runTool(t, NewReadTool(), ctx, map[string]any{"file_path": path}); err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(t, NewEditTool(), ctx, map[string]any{
		"file_path": path, "old_string": "externally changed", "new_string": "one",
	}); err != nil {
		t.Fatalf("edit after re-read failed: %v", err)
	}
}

func TestWriteOverwriteRequiresRead(t *testing.T) {
	ctx := freshCtx(t)
	path := filepath.Join(ctx.WorkingDir, "c.txt")
	if err := os.WriteFile(path, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Overwriting an existing unseen file is rejected.
	if _, err := runTool(t, NewWriteTool(), ctx, map[string]any{
		"file_path": path, "content": "new\n",
	}); err == nil || !strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("expected not-read rejection for overwrite, got %v", err)
	}

	// Creating a brand-new file needs no Read.
	newPath := filepath.Join(ctx.WorkingDir, "new.txt")
	if _, err := runTool(t, NewWriteTool(), ctx, map[string]any{
		"file_path": newPath, "content": "brand new\n",
	}); err != nil {
		t.Fatalf("new-file write should not require a read: %v", err)
	}

	// After a Read, overwriting succeeds.
	if _, err := runTool(t, NewReadTool(), ctx, map[string]any{"file_path": path}); err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(t, NewWriteTool(), ctx, map[string]any{
		"file_path": path, "content": "new\n",
	}); err != nil {
		t.Fatalf("overwrite after read failed: %v", err)
	}
}
