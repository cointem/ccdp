package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Poll cancellation only after the invocation has begun, without timing races.
type cancelAfterChecks struct {
	context.Context
	remaining int
}

func (c *cancelAfterChecks) Err() error {
	c.remaining--
	if c.remaining <= 0 {
		return context.Canceled
	}
	return nil
}

func TestFileSearchToolsHonorCancellation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "read.txt"), []byte(strings.Repeat("line\n", 1000)), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("dir-%02d", i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		tool Tool
		args map[string]any
	}{
		{NewReadTool(), map[string]any{"file_path": "read.txt", "offset": 999}},
		{NewGlobTool(), map[string]any{"pattern": "**/*"}},
		{NewLSTool(), map[string]any{}},
	} {
		for _, after := range []int{1, 20} {
			t.Run(fmt.Sprintf("%s/after%d", tc.tool.Name(), after), func(t *testing.T) {
				ctx := scopedTestContext(t, root)
				ctx.Context = &cancelAfterChecks{Context: context.Background(), remaining: after}
				ctx.Args = tc.args
				if out, err := tc.tool.Run(ctx); !errors.Is(err, context.Canceled) {
					t.Fatalf("returned %q, %v", out, err)
				}
			})
		}
	}
}

func TestWriteAndEditCancelAfterBeforeWrite(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprint(replace), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "file.txt")
			ctx := scopedTestContext(t, root)
			call, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx.Context = call
			var tool Tool = NewWriteTool()
			ctx.Args = map[string]any{"file_path": "file.txt", "content": "changed"}
			if replace {
				if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
					t.Fatal(err)
				}
				tool = NewEditTool()
				ctx.Args = map[string]any{"file_path": "file.txt", "edits": []any{map[string]any{"old_text": "original", "new_text": "changed"}}}
			}
			finished := false
			ctx.BeforeWrite = func(string, []byte) (func() error, error) {
				cancel()
				return func() error { finished = true; return nil }, nil
			}
			if _, err := tool.Run(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("got %v", err)
			}
			data, err := os.ReadFile(path)
			if replace {
				if err != nil || string(data) != "original" {
					t.Fatalf("published edit: %q %v", data, err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("published create: %q %v", data, err)
			}
			if finished {
				t.Fatal("completion callback ran for canceled publication")
			}
		})
	}
}

func TestWebURLValidationHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := validateExternalURL(ctx, "https://example.com"); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
