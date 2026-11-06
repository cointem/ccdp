package execution

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccdp/internal/sandbox"
)

func TestControllerProcessesAreSandboxedAndHostSurvives(t *testing.T) {
	c := NewController(t.TempDir() + "/runtime")
	crashed := false
	t.Cleanup(func() {
		if err := c.Close(); err != nil && !crashed {
			t.Error(err)
		}
	})
	root := t.TempDir()
	policy := sandbox.New(root)
	policy.ExecutorID = c.ID
	policy.SetScratchDir(t.TempDir())
	outside := filepath.Join(t.TempDir(), "forbidden")
	res, err := Run(Request{Context: context.Background(), Dir: root, Sandbox: policy,
		Command: "printf 'ready'; printf no > " + QuoteArg(outside), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 || !strings.Contains(res.Output, "ready") {
		t.Fatalf("unexpected result: %+v", res)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("escaped sandbox: %v", err)
	}
	p, err := StartShell(context.Background(), "sleep 30", root, nil, policy)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Start(); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	child := c.cmd.Process
	c.mu.Unlock()
	if err := child.Kill(); err != nil {
		t.Fatal(err)
	}
	crashed = true
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("lost executor did not settle invocation")
	}
	var unknown *OutcomeUnknownError
	if !errors.As(p.Wait(), &unknown) {
		t.Fatalf("lost invocation not unknown: %v", p.Wait())
	}
	// A dead generation never silently falls back or starts a new controller.
	if _, err := StartShell(context.Background(), "touch replay", root, nil, policy); err == nil {
		t.Fatal("dead executor restarted")
	}
}

func TestExecutorRejectsReplayAndKeepsControlAliveDuringOutput(t *testing.T) {
	root := t.TempDir()
	policy := sandbox.New(root)
	profile, err := policy.Profile()
	if err != nil {
		t.Fatal(err)
	}
	inR, inW := io.Pipe()
	controlR, controlW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- serveExecutor(inR, controlW, io.Discard); controlW.Close() }()
	defer func() { inW.Close(); controlR.Close(); <-done }()
	var hello wireMessage
	if err := readFrame(controlR, &hello); err != nil || hello.Op != "ready" || hello.Version != 2 {
		t.Fatalf("executor handshake: %+v %v", hello, err)
	}
	marker := filepath.Join(root, "effects")
	request := wireMessage{Seq: 1, ID: 1, Op: "start", Dir: root, Roots: []string{root}, Args: []string{sandbox.BackendPath, "-p", profile, "--", "/bin/sh", "-c", "printf x >> " + QuoteArg(marker)}}
	if err := writeFrame(inW, request); err != nil {
		t.Fatal(err)
	}
	var reply wireMessage
	if err := readFrame(controlR, &reply); err != nil || reply.Op != "started" {
		t.Fatalf("start: %+v %v", reply, err)
	}
	if err := readFrame(controlR, &reply); err != nil || reply.Op != "done" {
		t.Fatalf("done: %+v %v", reply, err)
	}
	if err := writeFrame(inW, request); err != nil {
		t.Fatal(err)
	}
	if err := readFrame(controlR, &reply); err != nil || !strings.Contains(reply.Error, "no replay") {
		t.Fatalf("duplicate: %+v %v", reply, err)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "x" {
		t.Fatalf("duplicate effect: %q %v", data, err)
	}
	request.Seq, request.ID = 2, 2
	request.Args[len(request.Args)-1] = "while :; do printf 'output flood\\n'; done"
	if err := writeFrame(inW, request); err != nil {
		t.Fatal(err)
	}
	if err := readFrame(controlR, &reply); err != nil || reply.Op != "started" {
		t.Fatalf("flood start: %+v %v", reply, err)
	}
	if err := writeFrame(inW, wireMessage{Seq: 3, ID: 2, Op: "stop", Reason: "cancelled"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := readFrame(controlR, &reply); err != nil {
			t.Fatal(err)
		}
		if reply.Op != "done" && reply.Op != "ack" {
			t.Fatalf("stop: %+v", reply)
		}
	}
}

func TestControllerKnownRejectionDoesNotPoisonEnvironment(t *testing.T) {
	c := NewController(t.TempDir() + "/runtime")
	defer c.Close()
	policy := sandbox.New(t.TempDir())
	policy.ExecutorID = c.ID
	policy.SetScratchDir(t.TempDir())
	p, err := StartShell(context.Background(), "touch should-not-run", policy.Workspace, nil, policy)
	if err != nil {
		t.Fatal(err)
	}
	p.cmd.Env = append(p.cmd.Env, "TOO_LARGE="+strings.Repeat("x", wireLimit))
	if err := p.Start(); err == nil {
		t.Fatal("oversized request accepted")
	} else {
		var unknown *OutcomeUnknownError
		if errors.As(err, &unknown) {
			t.Fatal("local preflight misreported as unknown")
		}
	}
	if policy.ExternalExecutionPossible() {
		t.Fatal("rejected before dispatch recorded an execution")
	}
	if _, err := os.Stat(filepath.Join(policy.Workspace, "should-not-run")); !os.IsNotExist(err) {
		t.Fatal("rejected request ran")
	}
	result, err := Run(Request{Context: context.Background(), Dir: policy.Workspace, Sandbox: policy, Command: "printf usable"})
	if err != nil || result.Output != "usable" {
		t.Fatalf("preflight poisoned generation: %+v %v", result, err)
	}
}

func TestExecutorCrashPersistsWritableScope(t *testing.T) {
	c := NewController(t.TempDir() + "/runtime")
	defer c.Close()
	policy := sandbox.New(t.TempDir())
	policy.ExecutorID = c.ID
	policy.SetScratchDir(t.TempDir())
	policy.QuarantineStore = filepath.Join(t.TempDir(), "quarantine.json")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p, err := StartShell(ctx, "sleep 30", policy.Workspace, nil, policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	process := c.cmd.Process
	c.mu.Unlock()
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(8 * time.Second):
		t.Fatal("crash did not settle")
	}
	restarted := sandbox.New(policy.Workspace)
	restarted.QuarantineStore = policy.QuarantineStore
	if err := restarted.CheckExecution(); err == nil {
		t.Fatal("restart silently dropped uncertainty")
	}
	if err := c.Close(); err == nil {
		t.Fatal("uncertain cleanup reported success")
	}
	if err := c.Close(); err == nil {
		t.Fatal("repeated close lost cleanup error")
	}
}
func TestControllerInteractiveProcess(t *testing.T) {
	c := NewController(t.TempDir() + "/runtime")
	defer c.Close()
	policy := sandbox.New(t.TempDir())
	policy.ExecutorID = c.ID
	policy.SetScratchDir(t.TempDir())
	p, err := StartShell(context.Background(), "cat", policy.Workspace, nil, policy)
	if err != nil {
		t.Fatal(err)
	}
	in, err := p.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var output limitedBuffer
	output.limit = 1000
	p.SetStderr(&output)
	p.cmd.Stdout = &output
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Write([]byte("hello executor\n")); err != nil {
		t.Fatal(err)
	}
	if err := in.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	if output.String() != "hello executor\n" {
		t.Fatal(output.String())
	}
}

func testSandbox(t *testing.T, root string) *sandbox.Sandbox {
	t.Helper()
	policy := sandbox.New(root)
	policy.SetScratchDir(t.TempDir())
	controller := NewController(t.TempDir() + "/runtime")
	policy.ExecutorID = controller.ID
	t.Cleanup(func() {
		if err := controller.Close(); err != nil {
			t.Errorf("close test executor: %v", err)
		}
	})
	return policy
}

func TestExecutionRequiresControllerBinding(t *testing.T) {
	policy := sandbox.New(t.TempDir())
	marker := filepath.Join(policy.Workspace, "must-not-run")
	command := "touch " + QuoteArg(marker)
	if _, err := Run(Request{Command: command, Dir: policy.Workspace, Sandbox: policy}); !errors.Is(err, ErrEnvironmentUnavailable) {
		t.Fatalf("unbound Run: %v", err)
	}
	if _, err := StartShell(context.Background(), command, policy.Workspace, nil, policy); !errors.Is(err, ErrEnvironmentUnavailable) {
		t.Fatalf("unbound StartShell: %v", err)
	}
	if _, err := StartArgv(StartRequest{Argv: []string{"/usr/bin/touch", marker}, Dir: policy.Workspace, Sandbox: policy}); !errors.Is(err, ErrEnvironmentUnavailable) {
		t.Fatalf("unbound StartArgv: %v", err)
	}
	if _, err := CodeWorker(context.Background(), "--probe"); !errors.Is(err, ErrEnvironmentUnavailable) {
		t.Fatalf("unbound CodeWorker: %v", err)
	}
	if err := Quarantine(context.Background(), policy); !errors.Is(err, ErrEnvironmentUnavailable) {
		t.Fatalf("unbound quarantine reported success: %v", err)
	}
	if err := ResumeAdmission(policy); !errors.Is(err, ErrEnvironmentUnavailable) {
		t.Fatalf("unbound resume reported success: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("unbound operation executed: %v", err)
	}
}

func TestArtifactTraversalDoesNotExposePrivateSessions(t *testing.T) {
	policy := testSandbox(t, t.TempDir())
	private := filepath.Join(t.TempDir(), "sessions")
	outputs := filepath.Join(private, "current", "outputs")
	other := filepath.Join(private, "other", "outputs")
	for _, dir := range []string{outputs, other} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "result.txt"), []byte("retained-output"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	journal := filepath.Join(private, "current", "events.jsonl")
	if err := os.WriteFile(journal, []byte("private-journal"), 0600); err != nil {
		t.Fatal(err)
	}
	policy.AddPrivateRoot(private)
	policy.AddArtifactRoot(outputs)
	run := func(command string) Result {
		t.Helper()
		result, err := Run(Request{Command: command, Dir: policy.Workspace, Sandbox: policy})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if result := run("cat " + QuoteArg(filepath.Join(outputs, "result.txt"))); result.ExitCode != 0 || result.Output != "retained-output" {
		t.Fatalf("authorized artifact inaccessible: %+v", result)
	}
	for _, command := range []string{
		"ls " + QuoteArg(private),
		"ls " + QuoteArg(filepath.Dir(outputs)),
		"cat " + QuoteArg(journal),
		"cat " + QuoteArg(filepath.Join(other, "result.txt")),
		"touch " + QuoteArg(filepath.Join(outputs, "must-not-write")),
	} {
		if result := run(command); result.ExitCode == 0 {
			t.Errorf("artifact traversal expanded access: %s", command)
		}
	}
	policy.AddDisallowedDir(private)
	if result := run("cat " + QuoteArg(filepath.Join(outputs, "result.txt"))); result.ExitCode == 0 {
		t.Fatal("artifact allowance bypassed explicit deny")
	}
}
