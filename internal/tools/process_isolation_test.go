package tools

import (
	"ccdp/internal/execution"
	"ccdp/internal/runtimepath"
	"ccdp/internal/sandbox"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestProcessIDErrorsDistinguishMalformedAndStaleHandles(t *testing.T) {
	generation, _ := runtimepath.ID()
	owner, _ := runtimepath.ID()
	m := NewProcessManager("test", generation, owner)
	id, err := m.newID()
	if err != nil {
		t.Fatal(err)
	}
	for _, malformed := range []string{"", "p.token", id[:len(id)-1], strings.Replace(id, "p.", "x.", 1)} {
		if _, err := m.Get(malformed); err == nil || !strings.Contains(err.Error(), "invalid_process_id") {
			t.Fatalf("malformed %q: %v", malformed, err)
		}
	}
	if _, err := m.Get(id); err == nil || !strings.Contains(err.Error(), "process_released") {
		t.Fatalf("well-formed missing handle: %v", err)
	}
	other, _ := runtimepath.ID()
	if _, err := m.Get(strings.Replace(id, generation, other, 1)); err == nil || !strings.Contains(err.Error(), "stale_process") {
		t.Fatalf("old generation: %v", err)
	}
	if _, err := m.Get(strings.Replace(id, owner, other, 1)); err == nil || !strings.Contains(err.Error(), "foreign_process") {
		t.Fatalf("other owner: %v", err)
	}
}

func TestUnixSocketCannotReachFutureSiblingEnvironment(t *testing.T) {
	for _, shared := range []bool{true, false} {
		t.Run(map[bool]string{true: "shared_executor", false: "separate_executor"}[shared], func(t *testing.T) {
			first := NewResources("first", "", t.TempDir()+"/runtime")
			defer first.Close()
			// Put both future environments under the workspace grant. The private
			// root must still exclude every owner except this command's own scratch.
			workspace := filepath.Dir(filepath.Dir(first.ScratchDir()))
			ctx := first.Context(context.Background(), workspace, sandbox.New(workspace))
			out, err := runTool(t, NewBashTool(), ctx, map[string]any{"command": `python3 -c 'import socket,sys; s=socket.socket(socket.AF_UNIX); s.connect(sys.stdin.readline().strip()); print("CONNECTED")'`, "stdin": "pipe"})
			process := decodeProcess(t, out, err)
			executor := ""
			if shared {
				executor = first.ExecutorID()
			}
			second := NewResourcesWithContext("second", "", context.Background(), executor, t.TempDir()+"/runtime")
			defer second.Close()
			if err := second.CheckOpen(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(second.ScratchDir(), "host.sock")
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			if _, err := runTool(t, NewProcessTool(), ctx, map[string]any{"id": process.ID, "action": "write", "input": path + "\n", "eof": true}); err != nil {
				t.Fatal(err)
			}
			out, err = runTool(t, NewProcessTool(), ctx, map[string]any{"id": process.ID, "action": "wait"})
			result := decodeProcess(t, out, err)
			if result.ExitCode == nil || *result.ExitCode == 0 || !strings.Contains(result.Output, "Operation not permitted") {
				t.Fatalf("old profile exposed future sibling socket: %+v", result)
			}
		})
	}
}

func TestPrivateRuntimeRejectsFutureSiblingScratch(t *testing.T) {
	for _, shared := range []bool{true, false} {
		t.Run(map[bool]string{true: "shared_executor", false: "separate_executor"}[shared], func(t *testing.T) { testPrivateRuntimeFutureSibling(t, shared) })
	}
}
func testPrivateRuntimeFutureSibling(t *testing.T, shared bool) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, "private-runtime")
	first := NewResources("first", "", root)
	defer first.Close()
	ctx := first.Context(context.Background(), workspace, sandbox.New(workspace))
	// This command's immutable profile predates the other owner/environment.
	out, err := runTool(t, NewBashTool(), ctx, map[string]any{"command": `python3 -u -c 'import sys; from pathlib import Path; print(Path(sys.stdin.readline().strip()).read_text())'`, "stdin": "pipe"})
	running := decodeProcess(t, out, err)
	executorID := ""
	if shared {
		executorID = first.ExecutorID()
	}
	second := NewResourcesWithContext("second", "", context.Background(), executorID, root)
	defer second.Close()
	if err := second.CheckOpen(); err != nil {
		t.Fatal(err)
	}
	const secret = "FUTURE_OWNER_PRIVATE_SENTINEL"
	path := filepath.Join(second.ScratchDir(), "secret")
	if err := os.WriteFile(path, []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	if result, err := NewReadTool().Execute(ctx, map[string]any{"file_path": path}); err == nil {
		t.Fatalf("Read crossed owner boundary: %s", result.Text)
	}
	if _, err := runTool(t, NewProcessTool(), ctx, map[string]any{"id": running.ID, "action": "write", "input": path + "\n", "eof": true}); err != nil {
		t.Fatal(err)
	}
	out, err = runTool(t, NewProcessTool(), ctx, map[string]any{"id": running.ID, "action": "wait"})
	done := decodeProcess(t, out, err)
	if strings.Contains(done.Output, secret) || !strings.Contains(done.Output, "Operation not permitted") {
		t.Fatalf("old profile did not isolate future scratch: %+v", done)
	}
	own := filepath.Join(first.ScratchDir(), "own")
	if err := os.WriteFile(own, []byte("own-readable"), 0600); err != nil {
		t.Fatal(err)
	}
	if result, err := NewReadTool().Execute(ctx, map[string]any{"file_path": own}); err != nil || !strings.Contains(result.Text, "own-readable") {
		t.Fatalf("own scratch not usable: %v %s", err, result.Text)
	}
	alias := filepath.Join(workspace, "alias")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := NewReadTool().Execute(ctx, map[string]any{"file_path": alias}); err == nil {
		t.Fatal("Read followed private alias")
	}
	out, err = runTool(t, NewBashTool(), ctx, map[string]any{"command": "cat " + execution.QuoteArg(alias)})
	if v := decodeProcess(t, out, err); v.ExitCode == nil || *v.ExitCode == 0 || strings.Contains(v.Output, secret) {
		t.Fatal("Bash followed private alias", v)
	}
	ctx.Sandbox.AddDisallowedDir(own)
	if _, err := NewReadTool().Execute(ctx, map[string]any{"file_path": own}); err == nil {
		t.Fatal("scratch grant overrode explicit deny")
	}
	out, err = runTool(t, NewBashTool(), ctx, map[string]any{"command": "cat " + execution.QuoteArg(own)})
	if v := decodeProcess(t, out, err); v.ExitCode == nil || *v.ExitCode == 0 || strings.Contains(v.Output, "own-readable") {
		t.Fatal("Bash scratch grant overrode explicit deny", v)
	}
	if _, err := ctx.Sandbox.ResolveWrite(path); err == nil {
		t.Fatal("workspace authority overrode private runtime")
	}
	want := "stale_process"
	if shared {
		want = "foreign_process"
	}
	if _, err := second.Processes.Get(running.ID); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatal("sibling accepted foreign handle", err)
	}
}

// Run two independent Go test processes, not just two managers in one runtime.
func TestProcessIDsCannotAliasAcrossOSProcesses(t *testing.T) {
	root := t.TempDir()
	for _, stage := range []string{"old", "new"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestProcessIDSubprocessHelper$", "-test.count=1")
		cmd.Env = append(os.Environ(), "CCDP_TEST_PROCESS_STAGE="+stage, "CCDP_TEST_PROCESS_ROOT="+root)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s helper: %v\n%s", stage, err, out)
		}
	}
}

func TestProcessIDSubprocessHelper(t *testing.T) {
	stage, root := os.Getenv("CCDP_TEST_PROCESS_STAGE"), os.Getenv("CCDP_TEST_PROCESS_ROOT")
	if stage == "" {
		t.Skip("subprocess helper")
	}
	r := NewResources("same-session", "", filepath.Join(root, "runtime"))
	defer r.Close()
	ctx := r.Context(context.Background(), root, sandbox.New(root))
	out, err := runTool(t, NewBashTool(), ctx, map[string]any{"command": "cat", "stdin": "pipe"})
	p := decodeProcess(t, out, err)
	path := filepath.Join(root, "old-id.json")
	if stage == "old" {
		data, _ := json.Marshal(p.ID)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var old string
	if err = json.Unmarshal(data, &old); err != nil {
		t.Fatal(err)
	}
	if old == p.ID {
		t.Fatal("new runtime reused an old handle")
	}
	if _, err = runTool(t, NewProcessTool(), ctx, map[string]any{"id": old, "action": "stop"}); err == nil || !strings.Contains(err.Error(), "stale_process") {
		t.Fatal("old stop was not rejected", err)
	}
	if _, err = runTool(t, NewProcessTool(), ctx, map[string]any{"id": p.ID, "action": "write", "input": "still-alive", "eof": true}); err != nil {
		t.Fatal(err)
	}
	out, err = runTool(t, NewProcessTool(), ctx, map[string]any{"id": p.ID, "action": "wait"})
	if done := decodeProcess(t, out, err); done.Output != "still-alive" {
		t.Fatal("old handle affected new process", done)
	}
}

func BenchmarkProcessAppend64KiB(b *testing.B) {
	var p managedProcess
	fragment := make([]byte, 64<<10)
	p.appendOutput(fragment)
	b.SetBytes(int64(len(fragment)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.appendOutput(fragment)
	}
}

func TestProcessBufferWrapAndAppendAllocations(t *testing.T) {
	var p managedProcess
	fragment := []byte(strings.Repeat("x", 8191))
	p.appendOutput(fragment)
	if allocs := testing.AllocsPerRun(1000, func() { p.appendOutput(fragment) }); allocs != 0 {
		t.Fatalf("append allocations: %v", allocs)
	}
	p.appendOutput([]byte("中文-end"))
	tail := p.tail.Tail(10)
	if string(tail) != "中文-end" || p.tail.Len() != maxProcOutput {
		t.Fatalf("wrapped tail: %q size %d", tail, p.tail.Len())
	}
}
