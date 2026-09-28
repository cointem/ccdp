package tools

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func ctxWithArgs(ctx *Context, args map[string]any) *Context {
	c := *ctx
	c.Args = args
	return &c
}
func extractPID(t *testing.T, out string) int { return decodeProcess(t, out, nil).ID }

func TestProcessDefaultStdinEOF(t *testing.T) {
	ctx := freshCtx(t)
	out, err := runTool(t, NewBashTool(), ctx, map[string]any{"command": "cat", "yield_ms": 1000})
	v := decodeProcess(t, out, err)
	if v.Status != "exited" || v.ExitCode == nil || *v.ExitCode != 0 {
		t.Fatal(v)
	}
	if _, err := runTool(t, NewProcessTool(), ctx, map[string]any{"id": v.ID, "action": "write", "input": "x"}); err == nil {
		t.Fatal("write accepted closed stdin")
	}
}

func TestProcessStopConcurrentAndDeletesLog(t *testing.T) {
	ctx := freshCtx(t)
	out, err := runTool(t, NewBashTool(), ctx, map[string]any{"command": `sh -c 'trap "" INT; sleep 30 & wait'`, "yield_ms": 0})
	v := decodeProcess(t, out, err)
	mp, err := ctx.Resources.Processes.Get(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, done, err := mp.stop(); err != nil || !done {
				t.Error(done, err)
			}
		}()
	}
	wg.Wait()
	ctx.Resources.Processes.Remove(v.ID)
	if _, err := os.Stat(v.Log); !os.IsNotExist(err) {
		t.Fatal("log retained", err)
	}
}

func TestProcessEOFIsIdempotent(t *testing.T) {
	ctx := freshCtx(t)
	out, err := runTool(t, NewBashTool(), ctx, map[string]any{"command": "cat", "stdin": "pipe", "yield_ms": 0})
	v := decodeProcess(t, out, err)
	for i := 0; i < 2; i++ {
		if _, err := runTool(t, NewProcessTool(), ctx, map[string]any{"id": v.ID, "action": "write", "eof": true}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProcessOwnerVersusObservationCancellation(t *testing.T) {
	ctx := freshCtx(t)
	call, cancel := context.WithCancel(context.Background())
	start := *ctx
	start.Context = call
	out, err := runTool(t, NewBashTool(), &start, map[string]any{"command": "cat", "stdin": "pipe", "yield_ms": 0})
	v := decodeProcess(t, out, err)
	cancel()
	if _, err := runTool(t, NewProcessTool(), ctx, map[string]any{"id": v.ID, "action": "write", "input": "alive\n"}); err != nil {
		t.Fatal(err)
	}
	if err := ctx.Resources.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(v.Log); !os.IsNotExist(err) {
		t.Fatal("owner retained log", err)
	}
}

func TestProcessTimeoutReason(t *testing.T) {
	ctx := freshCtx(t)
	out, err := runTool(t, NewBashTool(), ctx, map[string]any{"command": "sleep 30", "timeout_ms": 30, "yield_ms": 0})
	v := decodeProcess(t, out, err)
	mp, _ := ctx.Resources.Processes.Get(v.ID)
	select {
	case <-mp.done:
	case <-time.After(8 * time.Second):
		t.Fatal("timeout failed")
	}
	out, err = runTool(t, NewProcessTool(), ctx, map[string]any{"id": v.ID, "action": "read"})
	if got := decodeProcess(t, out, err); got.Status != "timed_out" {
		t.Fatal(got)
	}
}

func TestProcessBoundedBuffer(t *testing.T) {
	mp := &managedProcess{}
	payload := strings.Repeat("x", 2*maxProcOutput) + "end"
	mp.appendOutput([]byte(payload))
	if len(mp.buf) != maxProcOutput || !strings.HasSuffix(string(mp.buf), "end") || mp.total != int64(len(payload)) {
		t.Fatal("tail or total incorrect")
	}
}
