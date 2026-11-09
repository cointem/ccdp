package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccdp/internal/protocol"
)

func decodeProcess(t *testing.T, out string, e error) protocol.ProcessOutput {
	t.Helper()
	if e != nil {
		t.Fatal(e, out)
	}
	var v protocol.ProcessOutput
	if e = json.Unmarshal([]byte(out), &v); e != nil {
		t.Fatal(e, out)
	}
	return v
}
func TestUnifiedProcessRepeatableOutputAndEOF(t *testing.T) {
	ctx := freshCtx(t)
	out, e := runToolData(t, NewBashTool(), ctx, map[string]any{"command": "cat", "stdin": "pipe", "background": true})
	v := decodeProcess(t, out, e)
	id := v.ID
	if id == "" {
		t.Fatal(v)
	}
	if _, e = runToolData(t, NewProcessTool(), ctx, map[string]any{"id": id, "action": "write", "input": "中文 marker\n", "eof": true}); e != nil {
		t.Fatal(e)
	}
	mp, _ := ctx.Resources.Processes.Get(id)
	select {
	case <-mp.done:
	case <-time.After(3 * time.Second):
		t.Fatal("EOF did not end cat")
	}
	if n := ctx.Resources.Processes.Count(); n != 0 {
		t.Fatalf("exited processes counted: %d", n)
	}
	args := map[string]any{"id": id, "action": "read", "offset": 0}
	out, e = runToolData(t, NewProcessTool(), ctx, args)
	a := decodeProcess(t, out, e)
	out, e = runToolData(t, NewProcessTool(), ctx, args)
	b := decodeProcess(t, out, e)
	if a.Output != b.Output || !strings.Contains(a.Output, "中文 marker") || a.ExitCode == nil || *a.ExitCode != 0 {
		t.Fatal(a, b)
	}
}

func TestCompletedProcessRetentionIsBounded(t *testing.T) {
	m := NewProcessManager("retention", "generation", "owner")
	logs := t.TempDir()
	for i := 1; i <= maxCompletedProcesses+10; i++ {
		path := filepath.Join(logs, fmt.Sprintf("process-%d.log", i))
		if err := os.WriteFile(path, []byte("output"), 0600); err != nil {
			t.Fatal(err)
		}
		m.procs[fmt.Sprintf("%04d", i)] = &managedProcess{exited: true, done: closedProcessDone(), logPath: path}
	}
	m.procs["1000"] = &managedProcess{}
	m.mu.Lock()
	evicted := m.pruneExitedLocked()
	m.mu.Unlock()
	for _, p := range evicted {
		p.release()
	}
	if len(m.procs) != maxCompletedProcesses+1 || m.Count() != 1 {
		t.Fatal("incorrect retention", len(m.procs), m.Count())
	}
	if _, ok := m.procs["0001"]; ok {
		t.Fatal("oldest completed handle retained")
	}
	entries, err := os.ReadDir(logs)
	if err != nil || len(entries) != maxCompletedProcesses {
		t.Fatal("evicted logs retained", len(entries), err)
	}
	// An older, long-running command that finishes now must remain readable.
	m.procs["0001"] = &managedProcess{exited: true, done: closedProcessDone(), finishedAt: time.Now()}
	m.mu.Lock()
	evicted = m.pruneExitedLocked()
	m.mu.Unlock()
	for _, p := range evicted {
		p.release()
	}
	if _, ok := m.procs["0001"]; !ok {
		t.Fatal("newly completed long-running command evicted")
	}
}

func TestUnifiedProcessStopRemovesHandle(t *testing.T) {
	ctx := freshCtx(t)
	out, err := runToolData(t, NewBashTool(), ctx, map[string]any{"command": "cat", "stdin": "pipe", "background": true})
	v := decodeProcess(t, out, err)
	out, err = runToolData(t, NewProcessTool(), ctx, map[string]any{"id": v.ID, "action": "stop"})
	stopped := decodeProcess(t, out, err)
	if stopped.Log != "" || !strings.Contains(stopped.Warning, "released") || strings.Contains(stopped.Warning, "use Process") {
		t.Fatal("stop advertised an unavailable log/continuation", stopped)
	}
	if ctx.Resources.Processes.Count() != 0 {
		t.Fatal("stopped process remains active")
	}
	if _, err := ctx.Resources.Processes.Get(v.ID); err == nil {
		t.Fatal("stopped handle not removed")
	}
}
func TestCompletedLargeOutputCanContinue(t *testing.T) {
	ctx := freshCtx(t)
	out, e := runToolData(t, NewBashTool(), ctx, map[string]any{"command": "awk 'BEGIN { for(i=0;i<9000;i++) print \"abcdefghijk\" }'"})
	v := decodeProcess(t, out, e)
	mp, _ := ctx.Resources.Processes.Get(v.ID)
	select {
	case <-mp.done:
	case <-time.After(3 * time.Second):
		t.Fatal("command stuck")
	}
	var collected strings.Builder
	offset := 0
	for {
		out, e = runToolData(t, NewProcessTool(), ctx, map[string]any{"id": v.ID, "action": "read", "offset": offset})
		v = decodeProcess(t, out, e)
		collected.WriteString(v.Output)
		if v.Next == offset {
			break
		}
		offset = v.Next
	}
	if collected.Len() != 108000 {
		t.Fatal("lost output", collected.Len())
	}
}
func TestProcessWaitCancellationDoesNotKillProcess(t *testing.T) {
	ctx := freshCtx(t)
	out, e := runToolData(t, NewBashTool(), ctx, map[string]any{"command": "cat", "stdin": "pipe", "background": true})
	v := decodeProcess(t, out, e)
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	copy := *ctx
	copy.Context = cancelCtx
	for _, action := range []string{"read", "wait"} {
		if _, e = runToolData(t, NewProcessTool(), &copy, map[string]any{"id": v.ID, "action": action}); e == nil {
			t.Fatalf("%s ignored cancellation", action)
		}
	}
	if _, e = runToolData(t, NewProcessTool(), ctx, map[string]any{"id": v.ID, "action": "write", "input": "still alive\n", "eof": true}); e != nil {
		t.Fatal(e)
	}
}
func TestBashTTY(t *testing.T) {
	ctx := freshCtx(t)
	out, e := runToolData(t, NewBashTool(), ctx, map[string]any{"command": "test -t 1 && printf tty-ok", "tty": true})
	v := decodeProcess(t, out, e)
	mp, _ := ctx.Resources.Processes.Get(v.ID)
	<-mp.done
	out, e = runToolData(t, NewProcessTool(), ctx, map[string]any{"id": v.ID, "action": "read"})
	v = decodeProcess(t, out, e)
	if !strings.Contains(v.Output, "tty-ok") {
		t.Fatal(v)
	}
}

func TestProcessUTF8OffsetsAndInvalidBytes(t *testing.T) {
	ctx := freshCtx(t)
	out, e := runToolData(t, NewBashTool(), ctx, map[string]any{"command": "printf 'a中文b'"})
	v := decodeProcess(t, out, e)
	mp, _ := ctx.Resources.Processes.Get(v.ID)
	<-mp.done
	var result strings.Builder
	offset := 0
	for i := 0; i < 10; i++ {
		out, e = runToolData(t, NewProcessTool(), ctx, map[string]any{"id": v.ID, "action": "read", "offset": offset, "limit_bytes": 4})
		v = decodeProcess(t, out, e)
		result.WriteString(v.Output)
		if v.Next == offset {
			break
		}
		offset = v.Next
	}
	if result.String() != "a中文b" {
		t.Fatal(result.String())
	}
	out, e = runToolData(t, NewProcessTool(), ctx, map[string]any{"id": v.ID, "action": "read", "offset": 2, "limit_bytes": 4})
	v = decodeProcess(t, out, e)
	if v.Next <= 2 || !strings.Contains(v.Warning, "boundary") {
		t.Fatal(v)
	}
}

func closedProcessDone() chan struct{} { ch := make(chan struct{}); close(ch); return ch }
