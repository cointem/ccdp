package tools

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"ccdp/internal/execution"
	"ccdp/internal/sandbox"
)

// Interactive process tools let the agent drive long-running or interactive
// subprocesses (REPLs, dev servers, test watchers) across multiple tool calls.

// maxProcOutput caps the buffered output per process (old lines are dropped).
const maxProcOutput = 512 * 1024

const maxProcessWait = 30 * time.Second

// managedProcess is one running subprocess with its stdin pipe and a bounded
// output buffer.
type managedProcess struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	buf     []byte // bounded output buffer
	readPos int    // bytes already returned by ProcessOutput
	exited  bool
	err     error
	done    chan struct{}
}

// ProcessManager owns all background processes started by one Resources
// owner. Handles include a manager scope tag, so an integer returned by one
// session is rejected by every other manager even if both have a local id 1.
type ProcessManager struct {
	mu        sync.Mutex
	owner     string
	scope     uint32
	next      uint32
	procs     map[int]*managedProcess
	closed    bool
	closeErr  error
	closeDone chan struct{}
}

var processScopeSequence atomic.Uint32

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return err
		}
		return os.ErrProcessDone
	}
	cmd.WaitDelay = 3 * time.Second
}

// NewProcessManager creates an empty owner-scoped process manager.
func NewProcessManager(owner string) *ProcessManager {
	scope := processScopeSequence.Add(1) & 0x7fffffff
	if scope == 0 {
		scope = 1
	}
	return &ProcessManager{owner: owner, scope: scope, procs: map[int]*managedProcess{}, closeDone: make(chan struct{})}
}

// Owner returns the immutable scope identifier.
func (m *ProcessManager) Owner() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.owner
}

// CheckOpen verifies that the manager can still admit a process operation.
func (m *ProcessManager) CheckOpen() error {
	if m == nil {
		return fmt.Errorf("process: nil manager")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("process manager for %q is closed", m.owner)
	}
	return nil
}

// Start launches a long-running process. The command must already have passed
// sandbox/execution policy checks; ProcessStartTool calls sandbox.PrepareCommand
// immediately before this method.
func (m *ProcessManager) Start(command, dir string) (int, error) {
	id, _, err := m.startContext(context.Background(), command, dir)
	return id, err
}

func (m *ProcessManager) start(command, dir string) (int, *managedProcess, error) {
	return m.startContext(context.Background(), command, dir)
}

func (m *ProcessManager) startContext(ctx context.Context, command, dir string) (int, *managedProcess, error) {
	if m == nil {
		return 0, nil, fmt.Errorf("ProcessStart: nil process manager")
	}
	if strings.TrimSpace(command) == "" {
		return 0, nil, fmt.Errorf("ProcessStart: empty command")
	}
	if err := m.CheckOpen(); err != nil {
		return 0, nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.CommandContext(ctx, shell, "-c", command)
	cmd.Dir = dir
	cmd.Env = execution.SanitizedEnvironmentFor(execution.EnvironmentCommand, os.Environ())
	setProcessGroup(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 0, nil, fmt.Errorf("ProcessStart: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return 0, nil, fmt.Errorf("ProcessStart: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		return 0, nil, fmt.Errorf("ProcessStart: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return 0, nil, fmt.Errorf("ProcessStart: %w", err)
	}

	mp := &managedProcess{
		cmd:   cmd,
		stdin: stdin,
		done:  make(chan struct{}),
	}
	// Drain both streams concurrently: draining them sequentially can deadlock
	// when a process writes only to stderr.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		mp.pipe(stdout)
	}()
	go func() {
		defer wg.Done()
		mp.pipe(stderr)
	}()
	go func() {
		waitErr := cmd.Wait()
		wg.Wait()
		mp.mu.Lock()
		mp.err = waitErr
		mp.exited = true
		mp.mu.Unlock()
		close(mp.done)
	}()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_, _, _ = mp.stop()
		return 0, nil, fmt.Errorf("process manager for %q is closed", m.owner)
	}
	m.next++
	local := m.next
	id := int((uint64(m.scope) << 32) | uint64(local))
	m.procs[id] = mp
	m.mu.Unlock()
	return id, mp, nil
}

// pipe drains one output stream into a bounded buffer.
func (mp *managedProcess) pipe(r io.Reader) {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		fragment, err := br.ReadSlice('\n')
		if len(fragment) > 0 {
			mp.appendOutput(fragment)
		}
		if err != nil && err != bufio.ErrBufferFull {
			return
		}
	}
}

func (mp *managedProcess) appendOutput(fragment []byte) {
	if len(fragment) == 0 {
		return
	}
	mp.mu.Lock()
	mp.buf = append(mp.buf, fragment...)
	if len(mp.buf) > maxProcOutput {
		oldLen := len(mp.buf)
		mp.buf = append([]byte(nil), mp.buf[oldLen-maxProcOutput:]...)
		// Keep unread bytes contiguous after dropping old output.
		if dropped := oldLen - maxProcOutput; dropped > 0 {
			mp.readPos -= dropped
			if mp.readPos < 0 {
				mp.readPos = 0
			}
		}
	}
	mp.mu.Unlock()
}

// read returns output since the last read (bounded), plus exit state.
func (mp *managedProcess) read() (string, bool, error) {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	out := string(mp.buf[mp.readPos:])
	mp.readPos = len(mp.buf)
	return out, mp.exited, mp.err
}

// waitRead blocks until new output arrives, the process exits, or wait elapses.
func (mp *managedProcess) waitRead(wait time.Duration) (string, bool, error) {
	return mp.waitReadContext(context.Background(), wait)
}

// waitReadContext is the cancellable form used by ProcessOutput. A caller's
// interrupt must reclaim the wait promptly even when it requested the maximum
// 30-second observation window; the process itself remains session-owned and
// is not terminated by this per-call cancellation.
func (mp *managedProcess) waitReadContext(ctx context.Context, wait time.Duration) (string, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.Now().Add(wait)
	for {
		mp.mu.Lock()
		newOut := len(mp.buf) > mp.readPos
		exited := mp.exited
		mp.mu.Unlock()
		if newOut || exited || time.Now().After(deadline) {
			return mp.read()
		}
		select {
		case <-ctx.Done():
			return mp.read()
		default:
		}
		interval := 50 * time.Millisecond
		if remaining := time.Until(deadline); remaining < interval {
			interval = remaining
		}
		if interval <= 0 {
			return mp.read()
		}
		timer := time.NewTimer(interval)
		select {
		case <-mp.done:
			if !timer.Stop() {
				<-timer.C
			}
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return mp.read()
		case <-timer.C:
		}
	}
}

// write feeds one block to the process stdin.
func (mp *managedProcess) write(input string) error {
	if _, err := mp.stdin.Write([]byte(input)); err != nil {
		return fmt.Errorf("ProcessWrite: %w", err)
	}
	return nil
}

// stop terminates the process and returns remaining output + exit state.
func (mp *managedProcess) stop() (string, bool, error) {
	if mp == nil {
		return "", true, nil
	}
	_ = mp.stdin.Close()
	if mp.cmd.Process != nil {
		_ = syscall.Kill(-mp.cmd.Process.Pid, syscall.SIGINT)
	}
	select {
	case <-mp.done:
	case <-time.After(2 * time.Second):
		if mp.cmd.Process != nil {
			_ = syscall.Kill(-mp.cmd.Process.Pid, syscall.SIGKILL)
		}
		select {
		case <-mp.done:
		case <-time.After(4 * time.Second):
			return mp.read()
		}
	}
	return mp.read()
}

// Close stops every process owned by this manager and rejects future starts.
// It is idempotent; all calls return the same first close result.
func (m *ProcessManager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		done := m.closeDone
		m.mu.Unlock()
		<-done
		m.mu.Lock()
		err := m.closeErr
		m.mu.Unlock()
		return err
	}
	m.closed = true
	procs := make([]*managedProcess, 0, len(m.procs))
	for _, mp := range m.procs {
		procs = append(procs, mp)
	}
	m.procs = map[int]*managedProcess{}
	m.mu.Unlock()
	for _, mp := range procs {
		// A signal/exit status from an intentionally stopped background
		// process is not a Close failure. Close's contract is resource
		// reclamation; the process result is available through ProcessStop.
		_, _, _ = mp.stop()
	}
	m.mu.Lock()
	m.closeErr = nil
	close(m.closeDone)
	m.mu.Unlock()
	return nil
}

func (m *ProcessManager) belongs(pid int) bool {
	if pid <= 0 {
		return false
	}
	return uint64(pid)>>32 == uint64(m.scope)
}

// Get returns a live process by a handle created by this manager.
func (m *ProcessManager) Get(pid int) (*managedProcess, error) {
	if m == nil {
		return nil, fmt.Errorf("process: nil manager")
	}
	if !m.belongs(pid) {
		return nil, fmt.Errorf("process handle %d belongs to another session", pid)
	}
	m.mu.Lock()
	mp, ok := m.procs[pid]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no process %d (has it been stopped?)", pid)
	}
	return mp, nil
}

// Remove forgets a stopped process handle.
func (m *ProcessManager) Remove(pid int) {
	if m == nil || !m.belongs(pid) {
		return
	}
	m.mu.Lock()
	delete(m.procs, pid)
	m.mu.Unlock()
}

// ---------- ProcessStart ----------

// ProcessStartTool launches a long-running subprocess in the background.
type ProcessStartTool struct{}

// NewProcessStartTool creates the process start tool.
func NewProcessStartTool() *ProcessStartTool { return &ProcessStartTool{} }

func (t *ProcessStartTool) Name() string { return "ProcessStart" }

func (t *ProcessStartTool) Description() string {
	return `Start a long-running or interactive subprocess (REPL, dev server, watcher)
in the background and return its process id. The process keeps running between
tool calls: feed it input with ProcessWrite, read its output with ProcessOutput,
and terminate it with ProcessStop. Prefer this over Bash for anything that
blocks or waits for stdin.`
}

func (t *ProcessStartTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The command to start, e.g. 'python3 -i' or 'node server.js'.",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "A short note describing the process.",
			},
		},
		"required": []string{"command"},
	}
}

func (t *ProcessStartTool) Run(ctx *Context) (string, error) {
	if err := ctx.checkResources(); err != nil {
		return "", err
	}
	if ctx.Context != nil {
		select {
		case <-ctx.Context.Done():
			return "", ctx.Context.Err()
		default:
		}
	}
	command := StringArg(ctx.Args, "command", "")
	if strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("ProcessStart: empty command")
	}
	if err := sandbox.CheckInteractive(command); err != nil {
		return "", err
	}
	prepared, err := prepareCommand(ctx, command)
	if err != nil {
		return "", err
	}
	m, err := ctx.processManager()
	if err != nil {
		return "", err
	}
	// ProcessStart creates a session-owned long-lived process. The current tool
	// invocation may be canceled as soon as this step completes, so binding the
	// child to ctx.Context would kill a valid REPL/dev server at every boundary.
	// Resources.Close cancels OwnerContext and then closes the manager, retaining
	// the explicit session lifetime and shutdown semantics.
	ownerCtx := context.Background()
	if ctx.Resources != nil {
		ownerCtx = ctx.Resources.OwnerContext()
	}
	id, mp, err := m.startContext(ownerCtx, prepared, ctx.WorkingDir)
	if err != nil {
		return "", err
	}
	// Give the process a moment to print its banner, then surface it. The
	// manager remains asynchronous and owns the process after this call.
	time.Sleep(300 * time.Millisecond)
	out, _, _ := mp.read()
	if out != "" {
		out = "\nInitial output:\n" + out
	}
	return boundedProcessResult(ctx, fmt.Sprintf("Started process %d (%q).%s\nUse ProcessWrite to send input, ProcessOutput to read output, ProcessStop to end it.", id, command, out)), nil
}

// ---------- ProcessWrite ----------

// ProcessWriteTool writes input to a running process's stdin.
type ProcessWriteTool struct{}

// NewProcessWriteTool creates the process write tool.
func NewProcessWriteTool() *ProcessWriteTool { return &ProcessWriteTool{} }

func (t *ProcessWriteTool) Name() string { return "ProcessWrite" }

func (t *ProcessWriteTool) Description() string {
	return `Send input to a running process started with ProcessStart. The input is
written verbatim to the process stdin; add a trailing newline when the process
expects a line, e.g. to evaluate '2+2' in a Python REPL send "2+2\n".`
}

func (t *ProcessWriteTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pid": map[string]any{
				"type":        "integer",
				"description": "The process id returned by ProcessStart.",
			},
			"input": map[string]any{
				"type":        "string",
				"description": "The text to write to the process stdin.",
			},
		},
		"required": []string{"pid", "input"},
	}
}

func (t *ProcessWriteTool) Run(ctx *Context) (string, error) {
	if err := ctx.checkResources(); err != nil {
		return "", err
	}
	pid, err := IntArgChecked(ctx.Args, "pid", 0)
	if err != nil {
		return "", fmt.Errorf("ProcessWrite: %w", err)
	}
	input := StringArg(ctx.Args, "input", "")
	if len(input) > ctx.processInputLimit() {
		return "", fmt.Errorf("ProcessWrite: input is %d bytes, exceeds limit %d", len(input), ctx.processInputLimit())
	}
	m, err := ctx.processManager()
	if err != nil {
		return "", err
	}
	mp, err := m.Get(pid)
	if err != nil {
		return "", err
	}
	if err := mp.write(input); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to process %d", len(input), pid), nil
}

// ---------- ProcessOutput ----------

// ProcessOutputTool reads a running process's accumulated output.
type ProcessOutputTool struct{}

// NewProcessOutputTool creates the process output tool.
func NewProcessOutputTool() *ProcessOutputTool { return &ProcessOutputTool{} }

func (t *ProcessOutputTool) Name() string { return "ProcessOutput" }

func (t *ProcessOutputTool) Description() string {
	return `Read the output a process (started with ProcessStart) produced since the
last call. Optionally wait up to wait_ms milliseconds for new output before
returning. Returns the output and whether the process has exited. Output is
bounded; very long output is truncated from the front.`
}

func (t *ProcessOutputTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pid": map[string]any{
				"type":        "integer",
				"description": "The process id returned by ProcessStart.",
			},
			"wait_ms": map[string]any{
				"type":        "integer",
				"description": "Optional milliseconds to wait for new output (default 1500, maximum 30000).",
			},
		},
		"required": []string{"pid"},
	}
}

func (t *ProcessOutputTool) Run(ctx *Context) (string, error) {
	if err := ctx.checkResources(); err != nil {
		return "", err
	}
	pid, err := IntArgChecked(ctx.Args, "pid", 0)
	if err != nil {
		return "", fmt.Errorf("ProcessOutput: %w", err)
	}
	wait, err := IntArgChecked(ctx.Args, "wait_ms", 1500)
	if err != nil {
		return "", fmt.Errorf("ProcessOutput: %w", err)
	}
	if wait < 0 {
		wait = 0
	}
	if wait > int(maxProcessWait/time.Millisecond) {
		return "", fmt.Errorf("ProcessOutput: wait_ms exceeds maximum %d", int(maxProcessWait/time.Millisecond))
	}
	m, err := ctx.processManager()
	if err != nil {
		return "", err
	}
	mp, err := m.Get(pid)
	if err != nil {
		return "", err
	}
	out, exited, perr := mp.waitReadContext(ctx.Context, time.Duration(wait)*time.Millisecond)
	var sb strings.Builder
	sb.WriteString(out)
	if exited {
		status := "exited"
		if perr != nil {
			status = "failed: " + perr.Error()
		}
		fmt.Fprintf(&sb, "\n[process %d %s]\n", pid, status)
	} else {
		fmt.Fprintf(&sb, "\n[process %d still running]\n", pid)
	}
	return boundedProcessResult(ctx, sb.String()), nil
}

// ---------- ProcessStop ----------

// ProcessStopTool terminates a process and returns its remaining output.
type ProcessStopTool struct{}

// NewProcessStopTool creates the process stop tool.
func NewProcessStopTool() *ProcessStopTool { return &ProcessStopTool{} }

func (t *ProcessStopTool) Name() string { return "ProcessStop" }

func (t *ProcessStopTool) Description() string {
	return `Terminate a process started with ProcessStart (SIGINT, then SIGKILL after
2 seconds) and return its remaining output and exit state.`
}

func (t *ProcessStopTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pid": map[string]any{
				"type":        "integer",
				"description": "The process id returned by ProcessStart.",
			},
		},
		"required": []string{"pid"},
	}
}

func (t *ProcessStopTool) Run(ctx *Context) (string, error) {
	if err := ctx.checkResources(); err != nil {
		return "", err
	}
	pid, err := IntArgChecked(ctx.Args, "pid", 0)
	if err != nil {
		return "", fmt.Errorf("ProcessStop: %w", err)
	}
	m, err := ctx.processManager()
	if err != nil {
		return "", err
	}
	mp, err := m.Get(pid)
	if err != nil {
		return "", err
	}
	out, exited, perr := mp.stop()
	m.Remove(pid)
	var sb strings.Builder
	sb.WriteString(out)
	status := "stopped"
	if exited && perr == nil {
		status = "exited cleanly"
	} else if perr != nil {
		status = "failed: " + perr.Error()
	}
	fmt.Fprintf(&sb, "\n[process %d %s]\n", pid, status)
	return boundedProcessResult(ctx, sb.String()), nil
}

func boundedProcessResult(ctx *Context, value string) string {
	limit := ctx.outputLimit()
	if len(value) <= limit {
		return value
	}
	if limit <= len("\n…[tool output truncated]") {
		return value[:limit]
	}
	marker := "\n…[tool output truncated]"
	return value[:limit-len(marker)] + marker
}

func prepareCommand(ctx *Context, command string) (string, error) {
	if ctx == nil {
		return "", fmt.Errorf("execution: nil context")
	}
	if ctx.Sandbox == nil {
		return command, nil
	}
	return sandbox.PrepareCommand(ctx.Sandbox, command)
}
