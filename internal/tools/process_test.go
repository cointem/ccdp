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
func extractPID(t *testing.T, out string) string { return decodeProcess(t, out, nil).ID }

func TestProcessDefaultStdinEOF(t *testing.T) {
	ctx := freshCtx(t)
	out, err := runTool(t, NewBashTool(), ctx, map[string]any{"command": "cat"})
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
	out, err := runTool(t, NewBashTool(), ctx, map[string]any{"command": `sh -c 'trap "" INT; sleep 30 & wait'`, "background": true})
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
	out, err := runTool(t, NewBashTool(), ctx, map[string]any{"command": "cat", "stdin": "pipe", "background": true})
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
	out, err := runTool(t, NewBashTool(), &start, map[string]any{"command": "cat", "stdin": "pipe", "background": true})
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
	out, err := runTool(t, NewBashTool(), ctx, map[string]any{"command": "sleep 30", "timeout_ms": 30, "background": true})
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
	if mp.tail.Len() != maxProcOutput || !strings.HasSuffix(string(mp.tail.Tail(maxProcOutput)), "end") || mp.total != int64(len(payload)) {
		t.Fatal("tail or total incorrect")
	}
}

func TestBashConfiguredTimeoutAndExplicitOverride(t *testing.T) {
	for _, test := range []struct {
		name     string
		override any
		want     string
	}{
		{name: "configured", want: "timed_out"},
		{name: "explicit longer", override: 1000, want: "exited"},
		{name: "explicit unlimited", override: 0, want: "exited"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := freshCtx(t)
			ctx.Timeout = 20 * time.Millisecond
			args := map[string]any{"command": "sleep 0.2"}
			if test.override != nil {
				args["timeout_ms"] = test.override
			}
			out, err := runTool(t, NewBashTool(), ctx, args)
			view := decodeProcess(t, out, err)
			if view.Status != test.want {
				t.Fatalf("status=%s, want %s: %s", view.Status, test.want, out)
			}
		})
	}
}
