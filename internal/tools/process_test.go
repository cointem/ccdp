package tools

import (
	"os/exec"
	"strings"
	"testing"
)

func TestProcessLifecyclePython(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	ctx := &Context{WorkingDir: t.TempDir()}

	// Start an unbuffered python REPL-like loop.
	start := NewProcessStartTool()
	out, err := start.Run(ctxWithArgs(ctx, map[string]any{
		"command": "python3 -u -c 'import sys\nfor line in sys.stdin:\n    sys.stdout.write(\"got: \" + line)\n    sys.stdout.flush()'",
	}))
	if err != nil {
		t.Fatalf("ProcessStart: %v", err)
	}
	if !strings.Contains(out, "Started process") {
		t.Fatalf("start output malformed: %s", out)
	}
	pid := extractPID(t, out)

	write := NewProcessWriteTool()
	if _, err := write.Run(ctxWithArgs(ctx, map[string]any{
		"pid": pid, "input": "hello\n",
	})); err != nil {
		t.Fatalf("ProcessWrite: %v", err)
	}

	read := NewProcessOutputTool()
	out, err = read.Run(ctxWithArgs(ctx, map[string]any{"pid": pid, "wait_ms": 3000}))
	if err != nil {
		t.Fatalf("ProcessOutput: %v", err)
	}
	if !strings.Contains(out, "got: hello") {
		t.Errorf("process output missing echo: %s", out)
	}

	// Stop the process.
	stop := NewProcessStopTool()
	if _, err := stop.Run(ctxWithArgs(ctx, map[string]any{"pid": pid})); err != nil {
		t.Fatalf("ProcessStop: %v", err)
	}

	// The stopped process is gone.
	if _, err := read.Run(ctxWithArgs(ctx, map[string]any{"pid": pid})); err == nil {
		t.Error("expected error reading a stopped process")
	}
}

func TestProcessErrors(t *testing.T) {
	ctx := &Context{WorkingDir: t.TempDir()}

	if _, err := NewProcessStartTool().Run(ctxWithArgs(ctx, map[string]any{"command": "   "})); err == nil {
		t.Error("expected error for empty command")
	}
	if _, err := NewProcessWriteTool().Run(ctxWithArgs(ctx, map[string]any{"pid": 999, "input": "x"})); err == nil {
		t.Error("expected error for unknown pid")
	}
	if _, err := NewProcessOutputTool().Run(ctxWithArgs(ctx, map[string]any{"pid": 999})); err == nil {
		t.Error("expected error for unknown pid")
	}
	if _, err := NewProcessStopTool().Run(ctxWithArgs(ctx, map[string]any{"pid": 999})); err == nil {
		t.Error("expected error for unknown pid")
	}
}

func TestProcessBoundedBuffer(t *testing.T) {
	mp := &managedProcess{done: make(chan struct{})}
	// Simulate appends beyond the cap: old bytes are dropped, readPos stays sane.
	for i := 0; i < 100; i++ {
		mp.mu.Lock()
		mp.buf = append(mp.buf, strings.Repeat("x", maxProcOutput/10)...)
		if len(mp.buf) > maxProcOutput {
			mp.buf = append([]byte(nil), mp.buf[len(mp.buf)-maxProcOutput:]...)
			if mp.readPos > len(mp.buf) {
				mp.readPos = len(mp.buf)
			}
		}
		mp.mu.Unlock()
	}
	if len(mp.buf) > maxProcOutput {
		t.Errorf("buffer exceeded cap: %d", len(mp.buf))
	}
	// Reading the full buffer works and advances readPos to the end.
	out, _, _ := mp.read()
	if len(out) != len(mp.buf) {
		t.Errorf("read returned %d bytes, want %d", len(out), len(mp.buf))
	}
	if mp.readPos != len(mp.buf) {
		t.Errorf("readPos %d, want %d", mp.readPos, len(mp.buf))
	}
}

func ctxWithArgs(ctx *Context, args map[string]any) *Context {
	c := *ctx
	c.Args = args
	return &c
}

// extractPID parses "Started process <n>" from ProcessStart output.
func extractPID(t *testing.T, out string) int {
	t.Helper()
	const marker = "Started process "
	i := strings.Index(out, marker)
	if i < 0 {
		t.Fatalf("no pid marker in %q", out)
	}
	rest := out[i+len(marker):]
	end := strings.IndexAny(rest, " (\"\n")
	if end < 0 {
		end = len(rest)
	}
	var pid int
	for _, r := range rest[:end] {
		if r < '0' || r > '9' {
			break
		}
		pid = pid*10 + int(r-'0')
	}
	if pid == 0 {
		t.Fatalf("could not parse pid from %q", out)
	}
	return pid
}
