package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccdp/internal/execution"
	"ccdp/internal/sandbox"
)

func TestFileToolsRequireExecutor(t *testing.T) {
	ctx := scopedTestContext(t, t.TempDir())
	ctx.Sandbox.ExecutorID = ""
	for _, tc := range []struct {
		tool Tool
		args map[string]any
	}{
		{NewReadTool(), map[string]any{"file_path": "missing"}},
		{NewWriteTool(), map[string]any{"file_path": "must-not-write", "content": "no"}},
		{NewEditTool(), map[string]any{"file_path": "missing"}},
		{NewGlobTool(), map[string]any{"pattern": "*"}},
		{NewLSTool(), map[string]any{}},
	} {
		if _, err := tc.tool.Execute(ctx, tc.args); !errors.Is(err, execution.ErrEnvironmentUnavailable) {
			t.Errorf("%s ran without executor: %v", tc.tool.Name(), err)
		}
	}
	if _, err := os.Stat(filepath.Join(ctx.WorkingDir, "must-not-write")); !os.IsNotExist(err) {
		t.Fatalf("host file path executed: %v", err)
	}
}

func TestFileWorkerPreservesVersionAndMutationRecords(t *testing.T) {
	root := t.TempDir()
	r := NewResources("file-worker-test", "", t.TempDir()+"/runtime")
	defer r.Close()
	policy := sandbox.New(root)
	policy.ExecutorID = r.ExecutorID()
	ctx := r.Context(context.Background(), root, policy)
	path := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := NewWriteTool().Execute(ctx, map[string]any{"file_path": path, "content": "bad"})
	if err == nil || !strings.Contains(err.Error(), "not been read") {
		t.Fatalf("unobserved replacement: %v", err)
	}
	if _, err := NewReadTool().Execute(ctx, map[string]any{"file_path": path}); err != nil {
		t.Fatal(err)
	}
	before, after := 0, 0
	ctx.BeforeWrite = func(p string, data []byte) (func() error, error) {
		if p != path {
			return nil, errors.New("wrong path")
		}
		before++
		return func() error { after++; return nil }, nil
	}
	if _, err := NewWriteTool().Execute(ctx, map[string]any{"file_path": path, "content": "after\n"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "after\n" || before != 1 || after != 1 {
		t.Fatalf("mutation or record lost: %q %v %d/%d", b, err, before, after)
	}
	if err := os.WriteFile(path, []byte("external\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWriteTool().Execute(ctx, map[string]any{"file_path": path, "content": "stale"}); err == nil {
		t.Fatal("worker overwrote unseen version")
	}
	if _, err := (&LSTool{}).Execute(ctx, map[string]any{"path": root}); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(root, "blocked.txt")
	ctx.BeforeWrite = func(string, []byte) (func() error, error) { return nil, errors.New("journal unavailable") }
	if _, err := NewWriteTool().Execute(ctx, map[string]any{"file_path": blocked, "content": "must not publish"}); err == nil {
		t.Fatal("worker bypassed failed checkpoint")
	}
	if _, err := os.Stat(blocked); !os.IsNotExist(err) {
		t.Fatal("published without durable before-write acknowledgement")
	}
	ctx.BeforeWrite = func(string, []byte) (func() error, error) {
		return func() error { return errors.New("completion journal unavailable") }, nil
	}
	_, err = NewWriteTool().Execute(ctx, map[string]any{"file_path": blocked, "content": "published once"})
	var unknown *execution.OutcomeUnknownError
	if !errors.As(err, &unknown) {
		t.Fatalf("post-publication failure was retryable: %v", err)
	}
	if data, err := os.ReadFile(blocked); err != nil || string(data) != "published once" {
		t.Fatalf("publication fact lost: %q %v", data, err)
	}
}
