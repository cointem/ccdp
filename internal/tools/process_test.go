package tools

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccdp/internal/sandbox"
)

func TestProcessStopTerminatesProcessGroup(t *testing.T) {
	ctx := scopedTestContext(t, t.TempDir())
	out, err := NewProcessStartTool().Run(ctxWithArgs(ctx, map[string]any{
		"command": `sh -c 'trap "" INT; sleep 30 & wait'`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	pid := extractPID(t, out)
	start := time.Now()
	if _, err := NewProcessStopTool().Run(ctxWithArgs(ctx, map[string]any{"pid": pid})); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Fatalf("stopping process tree took %s", elapsed)
	}
}

func TestProcessLifecyclePython(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	ctx := scopedTestContext(t, t.TempDir())

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

func TestProcessStartSurvivesStepCancellationUntilOwnerClose(t *testing.T) {
	dir := t.TempDir()
	resources := NewResourcesWithContext("process-owner", filepath.Join(dir, "session"), context.Background())
	t.Cleanup(func() { _ = resources.Close() })

	stepCtx, cancelStep := context.WithCancel(context.Background())
	startCtx := resources.Context(stepCtx, dir, nil)
	startCtx.Args = map[string]any{"command": "sleep 30"}
	started, err := NewProcessStartTool().Run(startCtx)
	if err != nil {
		t.Fatalf("ProcessStart: %v", err)
	}
	pid := extractPID(t, started)
	cancelStep()

	readCtx := resources.Context(context.Background(), dir, nil)
	readCtx.Args = map[string]any{"pid": pid, "wait_ms": 0}
	out, err := NewProcessOutputTool().Run(readCtx)
	if err != nil {
		t.Fatalf("ProcessOutput after step cancellation: %v", err)
	}
	if !strings.Contains(out, "still running") {
		t.Fatalf("step cancellation killed session-owned process: %q", out)
	}

	if _, err := NewProcessStopTool().Run(readCtx); err != nil {
		t.Fatalf("ProcessStop: %v", err)
	}
}

func TestProcessOutputWaitHonorsContextCancellation(t *testing.T) {
	ctx := scopedTestContext(t, t.TempDir())
	started, err := NewProcessStartTool().Run(ctxWithArgs(ctx, map[string]any{"command": "sleep 30"}))
	if err != nil {
		t.Fatal(err)
	}
	pid := extractPID(t, started)
	readCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	readToolCtx := *ctx
	readToolCtx.Context = readCtx
	readToolCtx.Args = map[string]any{"pid": pid, "wait_ms": 30_000}
	done := make(chan error, 1)
	go func() {
		_, runErr := NewProcessOutputTool().Run(&readToolCtx)
		done <- runErr
	}()
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ProcessOutput after cancellation: %v", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("ProcessOutput ignored context cancellation for %s", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ProcessOutput remained blocked after context cancellation")
	}
	stopCtx := *ctx
	stopCtx.Args = map[string]any{"pid": pid}
	if _, err := NewProcessStopTool().Run(&stopCtx); err != nil {
		t.Fatal(err)
	}
}

func TestProcessStartAppliesSandboxLimits(t *testing.T) {
	ctx := scopedTestContext(t, t.TempDir())
	ctx.Sandbox = sandbox.New(ctx.WorkingDir, sandbox.ModeConfine)
	ctx.Sandbox.Limits = &sandbox.Limits{MaxFiles: 64}
	started, err := NewProcessStartTool().Run(ctxWithArgs(ctx, map[string]any{
		"command": "printf 'open-files=%s\\n' \"$(ulimit -n)\"",
	}))
	if err != nil {
		t.Fatal(err)
	}
	pid := extractPID(t, started)
	if !strings.Contains(started, "open-files=64") {
		t.Fatalf("ProcessStart did not apply MaxFiles limit: %q", started)
	}
	stopCtx := *ctx
	stopCtx.Args = map[string]any{"pid": pid}
	if _, err := NewProcessStopTool().Run(&stopCtx); err != nil {
		t.Fatal(err)
	}
}

func TestProcessErrors(t *testing.T) {
	ctx := scopedTestContext(t, t.TempDir())

	if _, err := NewProcessStartTool().Run(ctxWithArgs(ctx, map[string]any{"command": "   "})); err == nil {
		t.Error("expected error for empty command")
	}
	if _, err := NewProcessWriteTool().Run(ctxWithArgs(ctx, map[string]any{"pid": 999, "input": "x"})); err == nil {
		t.Error("expected error for unknown pid")
	}
	if _, err := NewProcessOutputTool().Run(ctxWithArgs(ctx, map[string]any{"pid": 999})); err == nil {
		t.Error("expected error for unknown pid")
	}
	if _, err := NewProcessOutputTool().Run(ctxWithArgs(ctx, map[string]any{"pid": 999, "wait_ms": 31_000})); err == nil || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Errorf("expected bounded wait_ms rejection, got %v", err)
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

func TestProcessTruncationKeepsUnreadContiguous(t *testing.T) {
	mp := &managedProcess{done: make(chan struct{})}
	// Feed more than maxProcOutput through the real pipe() path, consume it
	// all (readPos reaches the end), then feed another chunk: the consumer
	// must see exactly that chunk — no dropped, skipped or duplicated bytes.
	chunk1 := makeLines(0, 4500)
	mp.pipe(strings.NewReader(chunk1))
	out1, _, _ := mp.read()
	if len(out1) != maxProcOutput {
		t.Fatalf("first read = %d bytes, want capped %d", len(out1), maxProcOutput)
	}
	chunk2 := makeLines(4500, 4600)
	mp.pipe(strings.NewReader(chunk2))
	out2, _, _ := mp.read()
	if out2 != chunk2 {
		t.Errorf("second read = %d bytes, want the full %d-byte chunk uninterrupted", len(out2), len(chunk2))
	}
	if mp.readPos != len(mp.buf) {
		t.Errorf("readPos %d, want %d", mp.readPos, len(mp.buf))
	}
}

func TestProcessPipeDrainsUnterminatedLargeFragment(t *testing.T) {
	mp := &managedProcess{done: make(chan struct{})}
	// Scanner-based drains stop at their maximum token size and leave the
	// child blocked on a full pipe. ReadSlice must keep consuming a single
	// unterminated line while retaining only the bounded tail.
	payload := strings.Repeat("z", 2*1024*1024)
	mp.pipe(strings.NewReader(payload))
	out, _, _ := mp.read()
	if len(out) != maxProcOutput {
		t.Fatalf("unterminated output = %d bytes, want %d", len(out), maxProcOutput)
	}
	if strings.Trim(out, "z") != "" {
		t.Fatal("bounded output contained unexpected bytes")
	}
}

// makeLines builds a deterministic "NNNN aaaa…\n" block of to-from lines.
func makeLines(from, to int) string {
	var sb strings.Builder
	for i := from; i < to; i++ {
		fmt.Fprintf(&sb, "%04d %s\n", i, strings.Repeat("a", 120))
	}
	return sb.String()
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
