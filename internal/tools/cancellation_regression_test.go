package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Observe entry into a cancellable wait without changing context semantics.
type waitObservedContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *waitObservedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
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
		t.Run(tc.tool.Name(), func(t *testing.T) {
			ctx := scopedTestContext(t, root)
			call, cancel := context.WithCancel(context.Background())
			defer cancel()
			observed := &waitObservedContext{Context: call, waiting: make(chan struct{})}
			ctx.Context, ctx.Args = observed, tc.args
			ctx.Resources.initFileWorkers()
			for i := 0; i < cap(ctx.Resources.fileWorkers); i++ {
				<-ctx.Resources.fileWorkers
			}
			defer func() {
				for i := 0; i < cap(ctx.Resources.fileWorkers); i++ {
					ctx.Resources.fileWorkers <- nil
				}
			}()
			done := make(chan error, 1)
			go func() { _, err := executeToolText(tc.tool, ctx); done <- err }()
			select {
			case <-observed.waiting:
			case err := <-done:
				t.Fatalf("invocation did not wait for a worker: %v", err)
			case <-time.After(10 * time.Second):
				t.Fatal("worker admission did not become cancellable")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("got %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancelled admission did not settle")
			}
		})
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
				if _, err := NewReadTool().Execute(ctx, map[string]any{"file_path": "file.txt"}); err != nil {
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
			if _, err := executeToolText(tool, ctx); !errors.Is(err, context.Canceled) {
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
