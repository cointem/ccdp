package tools

import (
	"strings"
	"testing"
	"time"
)

func TestCommandToolRunsWithArgsOnStdin(t *testing.T) {
	dir := t.TempDir()
	tool := NewCommandTool(
		"echo-name",
		"echo the name",
		`cat /dev/stdin | python3 -c "import json,sys; print('hello ' + json.load(sys.stdin).get('name',''))"`,
		nil,
	)
	ctx := &Context{
		WorkingDir: dir,
		Args:       map[string]any{"name": "world"},
	}
	out, err := tool.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "hello world") {
		t.Errorf("unexpected output %q", out)
	}
}

func TestCommandToolFailureReported(t *testing.T) {
	dir := t.TempDir()
	tool := NewCommandTool("fail", "always fails", "exit 3", nil)
	ctx := &Context{WorkingDir: dir, Args: map[string]any{}}
	out, err := tool.Run(ctx)
	if err != nil {
		t.Fatalf("Run should not error, got %v", err)
	}
	if !strings.Contains(out, "failed") {
		t.Errorf("expected failure note, got %q", out)
	}
}

func TestCommandToolTimeout(t *testing.T) {
	dir := t.TempDir()
	tool := NewCommandTool("sleeper", "sleeps", "sleep 5", nil)
	ctx := &Context{
		WorkingDir: dir,
		Args:       map[string]any{},
		Timeout:    50 * time.Millisecond, // much shorter than the 5s sleep
	}
	out, err := tool.Run(ctx)
	if err != nil {
		t.Fatalf("timeout should return output, got %v", err)
	}
	if !strings.Contains(out, "timed out") {
		t.Errorf("expected timeout note, got %q", out)
	}
}
