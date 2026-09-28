package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLargeFilePartialReadAllowsExplicitReplace(t *testing.T) {
	ctx := freshCtx(t)
	p := filepath.Join(ctx.WorkingDir, "large")
	if err := os.WriteFile(p, []byte(strings.Repeat("line with enough text\n", 4000)), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(t, NewReadTool(), ctx, map[string]any{"file_path": p, "offset": 2000, "limit": 1}); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"replacement", "own second replacement"} {
		if _, err := runTool(t, NewWriteTool(), ctx, map[string]any{"file_path": p, "mode": "replace", "content": content}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEditRechecksAtPublicationWithoutOverwritingObserver(t *testing.T) {
	ctx := freshCtx(t)
	p := filepath.Join(ctx.WorkingDir, "file")
	_ = os.WriteFile(p, []byte("old text\n"), 0644)
	ctx.BeforeWrite = func(path string, knownBefore []byte) (func() error, error) {
		if string(knownBefore) != "old text\n" {
			t.Fatal("Edit did not reuse observed content")
		}
		return func() error { t.Fatal("failed publication attributed external bytes as our mutation"); return nil }, os.WriteFile(path, []byte("external change\n"), 0644)
	}
	_, err := runTool(t, NewEditTool(), ctx, map[string]any{"file_path": p, "edits": []any{map[string]any{"old_text": "old", "new_text": "new"}}})
	if err == nil {
		t.Fatal("concurrent edit overwritten")
	}
	b, _ := os.ReadFile(p)
	if string(b) != "external change\n" {
		t.Fatal(string(b))
	}
}
