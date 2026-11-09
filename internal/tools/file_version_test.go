package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteCreatesAndUpdatesFiles(t *testing.T) {
	for _, name := range []string{"new", "existing"} {
		t.Run(name, func(t *testing.T) {
			ctx := freshCtx(t)
			p := filepath.Join(ctx.WorkingDir, "file")
			if name == "existing" {
				if err := os.WriteFile(p, []byte(strings.Repeat("line with enough text\n", 4000)), 0644); err != nil {
					t.Fatal(err)
				}
				if _, err := runTool(t, NewReadTool(), ctx, map[string]any{"file_path": p, "offset": 2000, "limit": 1}); err != nil {
					t.Fatal(err)
				}
			}
			for _, content := range []string{"replacement", "own second replacement", ""} {
				if _, err := runTool(t, NewWriteTool(), ctx, map[string]any{"file_path": p, "content": content}); err != nil {
					t.Fatal(err)
				}
				if b, err := os.ReadFile(p); err != nil || string(b) != content {
					t.Fatalf("write lost: %q %v", b, err)
				}
			}
		})
	}
}

func TestFileMutationRechecksAtPublication(t *testing.T) {
	for _, name := range []string{"create", "replace", "edit"} {
		t.Run(name, func(t *testing.T) {
			ctx := freshCtx(t)
			p := filepath.Join(ctx.WorkingDir, "file")
			if name != "create" {
				if err := os.WriteFile(p, []byte("old text\n"), 0644); err != nil {
					t.Fatal(err)
				}
				if _, err := runTool(t, NewReadTool(), ctx, map[string]any{"file_path": p}); err != nil {
					t.Fatal(err)
				}
			}
			ctx.BeforeWrite = func(path string, knownBefore []byte) (func() error, error) {
				if name == "edit" && string(knownBefore) != "old text\n" {
					t.Fatal("Edit did not reuse observed content")
				}
				return func() error { t.Error("failed publication attributed external bytes as our mutation"); return nil }, os.WriteFile(path, []byte("external change\n"), 0644)
			}
			var tool Tool = NewWriteTool()
			args := map[string]any{"file_path": p, "content": "replacement"}
			if name == "edit" {
				tool, args = NewEditTool(), editArgs(p, "old", "new")
			}
			if _, err := runTool(t, tool, ctx, args); err == nil {
				t.Fatal("concurrent change overwritten")
			}
			if b, err := os.ReadFile(p); err != nil || string(b) != "external change\n" {
				t.Fatalf("external change lost: %q %v", b, err)
			}
		})
	}
}
