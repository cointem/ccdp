package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"ccdp/internal/protocol"
)

func TestBashForegroundStreamsWithoutYieldingToModel(t *testing.T) {
	ctx := freshCtx(t)
	progress := make(chan string, 16)
	ctx.ProcessProgress = func(_ protocol.ProcessProgress, s string) { progress <- s }
	type result struct {
		text string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		out, err := runTool(t, NewBashTool(), ctx, map[string]any{"command": "printf first; sleep 0.8; printf last"})
		done <- result{out, err}
	}()
	select {
	case text := <-progress:
		if !strings.Contains(text, "first") {
			t.Fatal(text)
		}
		select {
		case out := <-done:
			t.Fatalf("first output yielded the tool: %+v", out)
		default:
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no live progress")
	}
	select {
	case out := <-done:
		p := decodeProcess(t, out.text, out.err)
		if !p.Terminal() || p.Output != "firstlast" || p.ExitCode == nil || *p.ExitCode != 0 {
			t.Fatal(p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("command did not finish")
	}
}

func TestBashForegroundCancellationReclaimsProcess(t *testing.T) {
	ctx := freshCtx(t)
	call, cancel := context.WithCancel(ctx.Context)
	defer cancel()
	ctx.Context = call
	ctx.ProcessProgress = func(protocol.ProcessProgress, string) { cancel() }
	out, err := runTool(t, NewBashTool(), ctx, map[string]any{"command": "printf started; sleep 30"})
	if err == nil || ctx.Resources.Processes.Count() != 0 {
		t.Fatalf("foreground survived cancellation: %v %s", err, out)
	}
}

func TestBashUndeliverableHandleReclaimsProcess(t *testing.T) {
	ctx := freshCtx(t)
	ctx.OutputLimit = 32
	_, err := runTool(t, NewBashTool(), ctx, map[string]any{"command": "cat", "stdin": "pipe"})
	if err == nil || !strings.Contains(err.Error(), "stopped and released") || ctx.Resources.Processes.Count() != 0 {
		t.Fatalf("undeliverable process left running: %v", err)
	}
}

func TestBashBackgroundCompletionNotifiesWithoutReads(t *testing.T) {
	ctx := freshCtx(t)
	ctx.ProcessOrigin = "tool:turn-1:step-1:bash"
	completed := make(chan protocol.ProcessState, 4)
	ctx.ProcessEvent = func(e protocol.ProcessEvent) (string, error) {
		if e.Kind == "finished" {
			completed <- e.State
		}
		return "", nil
	}
	out, err := runTool(t, NewBashTool(), ctx, map[string]any{"command": "cat", "stdin": "pipe"})
	p := decodeProcess(t, out, err)
	if p.Terminal() {
		t.Fatal(p)
	}
	_, err = runTool(t, NewProcessTool(), ctx, map[string]any{"id": p.ID, "action": "write", "input": "result\n", "eof": true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case finished := <-completed:
		if finished.ID != p.ID || finished.Origin != ctx.ProcessOrigin || finished.Output != "result\n" || !finished.Terminal() {
			t.Fatal(finished)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no completion without model polling")
	}
}

func TestProcessWaitSettlesAfterDurableCompletion(t *testing.T) {
	ctx := freshCtx(t)
	entered, ack := make(chan struct{}), make(chan struct{})
	ctx.ProcessEvent = func(e protocol.ProcessEvent) (string, error) {
		if e.Kind == "finished" {
			close(entered)
			<-ack
		}
		return "saved-output", nil
	}
	out, err := runTool(t, NewBashTool(), ctx, map[string]any{"command": "cat", "stdin": "pipe"})
	p := decodeProcess(t, out, err)
	mp, _ := ctx.Resources.Processes.Get(p.ID)
	if err := mp.cmd.CloseInput(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("missing completion")
	}
	done := make(chan Result, 1)
	go func() {
		r, e := NewProcessTool().Execute(ctx, map[string]any{"id": p.ID, "action": "wait"})
		if e != nil {
			t.Error(e)
		}
		done <- r
	}()
	select {
	case <-done:
		t.Fatal("wait returned before owner committed")
	case <-time.After(30 * time.Millisecond):
	}
	close(ack)
	select {
	case r := <-done:
		if r.Process == nil || !r.Process.Terminal() || r.Process.OutputID != "saved-output" || len(r.CompletionIDs) != 1 {
			t.Fatal(r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not finish")
	}
}

func TestProcessReadIsImmediateAndRejectsWaitArguments(t *testing.T) {
	ctx := freshCtx(t)
	out, err := runTool(t, NewBashTool(), ctx, map[string]any{"command": "cat", "stdin": "pipe"})
	p := decodeProcess(t, out, err)
	start := time.Now()
	out, err = runTool(t, NewProcessTool(), ctx, map[string]any{"id": p.ID, "action": "read"})
	if read := decodeProcess(t, out, err); read.Terminal() || time.Since(start) > 250*time.Millisecond {
		t.Fatal("read waited", read)
	}
	for _, args := range []map[string]any{{"id": p.ID, "action": "read", "wait_ms": 1}, {"id": p.ID, "action": "wait", "offset": 0}, {"id": p.ID, "action": "stop", "input": "x"}} {
		if _, err := NewProcessTool().Execute(ctx, args); err == nil {
			t.Fatal("invalid argument accepted", args)
		}
	}
}

func TestBashFailureKeepsDiagnosticTailInBoundedResult(t *testing.T) {
	ctx := freshCtx(t)
	out, err := runTool(t, NewBashTool(), ctx, map[string]any{"command": "awk 'BEGIN { for(i=0;i<20000;i++) print \"ordinary progress\"; print \"FINAL FAILURE\"; exit 1 }'"})
	p := decodeProcess(t, out, err)
	if !p.Truncated || p.ExitCode == nil || *p.ExitCode != 1 || !strings.Contains(p.Tail, "FINAL FAILURE") || len(out) > 14000 {
		t.Fatalf("lost bounded failure diagnostic: bytes=%d tail=%q", len(out), p.Tail)
	}
}
