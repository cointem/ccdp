package tools

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Interactive process tools let the agent drive long-running or interactive
// subprocesses (REPLs, dev servers, test watchers) across multiple tool calls:
// start a process, feed it stdin lines, read its output, stop it. This is the
// missing half of Bash: commands that never return within a tool timeout.

// maxProcOutput caps the buffered output per process (old lines are dropped).
const maxProcOutput = 512 * 1024

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

// ProcessManager owns all background processes started this session.
type ProcessManager struct {
	mu    sync.Mutex
	next  int
	procs map[int]*managedProcess
}

var processStore = &ProcessManager{procs: map[int]*managedProcess{}}

func (m *ProcessManager) start(command, dir string) (int, *managedProcess, error) {
	if strings.TrimSpace(command) == "" {
		return 0, nil, fmt.Errorf("ProcessStart: empty command")
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell, "-c", command)
	cmd.Dir = dir
	cmd.Env = os.Environ()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 0, nil, fmt.Errorf("ProcessStart: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, nil, fmt.Errorf("ProcessStart: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 0, nil, fmt.Errorf("ProcessStart: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return 0, nil, fmt.Errorf("ProcessStart: %w", err)
	}

	mp := &managedProcess{
		cmd:   cmd,
		stdin: stdin,
		done:  make(chan struct{}),
	}
	// Drain both streams concurrently: draining them sequentially deadlocks
	// when a process writes only to stderr — stdout never reaches EOF until
	// the process exits, the pipe goroutine never reaches stderr, the 64KB
	// stderr pipe fills, and the blocked process never exits.
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
		defer close(mp.done)
		wg.Wait()
		mp.err = cmd.Wait()
		mp.mu.Lock()
		mp.exited = true
		mp.mu.Unlock()
	}()

	m.mu.Lock()
	m.next++
	id := m.next
	m.procs[id] = mp
	m.mu.Unlock()
	return id, mp, nil
}

// pipe drains one output stream into the bounded buffer.
func (mp *managedProcess) pipe(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		mp.mu.Lock()
		mp.buf = append(mp.buf, line...)
		mp.buf = append(mp.buf, '\n')
		if len(mp.buf) > maxProcOutput {
			oldLen := len(mp.buf)
			mp.buf = append([]byte(nil), mp.buf[oldLen-maxProcOutput:]...)
			// Shift readPos by the number of dropped bytes (computed before
			// truncation) so unread output stays contiguous — no bytes are
			// silently skipped and newly written lines are still returned.
			if dropped := oldLen - maxProcOutput; dropped > 0 {
				mp.readPos -= dropped
				if mp.readPos < 0 {
					mp.readPos = 0
				}
			}
		}
		mp.mu.Unlock()
	}
}

// read returns output since the last read (bounded), plus exit state.
func (mp *managedProcess) read() (string, bool, error) {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	out := string(mp.buf[mp.readPos:])
	mp.readPos = len(mp.buf)
	return out, mp.exited, mp.err
}

// waitRead blocks until new output arrives, the process exits, or wait elapses,
// then returns what is available. A short default wait avoids empty reads right
// after a ProcessWrite.
func (mp *managedProcess) waitRead(wait time.Duration) (string, bool, error) {
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
		case <-mp.done:
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// write feeds one line (or block) to the process stdin.
func (mp *managedProcess) write(input string) error {
	if _, err := mp.stdin.Write([]byte(input)); err != nil {
		return fmt.Errorf("ProcessWrite: %w", err)
	}
	return nil
}

// stop terminates the process and returns the remaining output + exit state.
func (mp *managedProcess) stop() (string, bool, error) {
	_ = mp.stdin.Close()
	if mp.cmd.Process != nil {
		_ = mp.cmd.Process.Signal(os.Interrupt)
	}
	select {
	case <-mp.done:
	case <-time.After(2 * time.Second):
		if mp.cmd.Process != nil {
			_ = mp.cmd.Process.Kill()
		}
		<-mp.done
	}
	return mp.read()
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
	command := StringArg(ctx.Args, "command", "")
	if ctx.Sandbox != nil {
		// Sandbox command policy (strict mode blocks workspace escapes + network).
		if err := ctx.Sandbox.CommandPolicy(command); err != nil {
			return "", err
		}
	}
	cmdline := command
	if ctx.Sandbox != nil {
		// Resource limits (ulimit prefix) and, on macOS with strict mode, a
		// sandbox-exec profile wrap the command — mirroring BashTool so the
		// background process can't bypass the sandbox.
		if p := ctx.Sandbox.Prefix(); p != "" {
			cmdline = p + " " + cmdline
		}
		if wrapped := ctx.Sandbox.WrapCommand(cmdline); wrapped != "" {
			cmdline = wrapped
		}
	}
	id, mp, err := processStore.start(cmdline, ctx.WorkingDir)
	if err != nil {
		return "", err
	}
	// Give the process a moment to print its banner, then surface it.
	time.Sleep(300 * time.Millisecond)
	out, _, _ := mp.read()
	if out != "" {
		out = "\nInitial output:\n" + out
	}
	return fmt.Sprintf("Started process %d (%q).%s\nUse ProcessWrite to send input, ProcessOutput to read output, ProcessStop to end it.",
		id, command, out), nil
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
	pid := IntArg(ctx.Args, "pid", 0)
	input := StringArg(ctx.Args, "input", "")
	mp, err := processStore.get(pid)
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
				"description": "Optional milliseconds to wait for new output (default 1500).",
			},
		},
		"required": []string{"pid"},
	}
}

func (t *ProcessOutputTool) Run(ctx *Context) (string, error) {
	pid := IntArg(ctx.Args, "pid", 0)
	wait := IntArg(ctx.Args, "wait_ms", 1500)
	if wait < 0 {
		wait = 0
	}
	mp, err := processStore.get(pid)
	if err != nil {
		return "", err
	}
	out, exited, perr := mp.waitRead(time.Duration(wait) * time.Millisecond)
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
	return sb.String(), nil
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
	pid := IntArg(ctx.Args, "pid", 0)
	mp, err := processStore.get(pid)
	if err != nil {
		return "", err
	}
	out, exited, perr := mp.stop()
	processStore.remove(pid)
	var sb strings.Builder
	sb.WriteString(out)
	status := "stopped"
	if exited && perr == nil {
		status = "exited cleanly"
	} else if perr != nil {
		status = "failed: " + perr.Error()
	}
	fmt.Fprintf(&sb, "\n[process %d %s]\n", pid, status)
	return sb.String(), nil
}

// get returns a live process by id.
func (m *ProcessManager) get(pid int) (*managedProcess, error) {
	m.mu.Lock()
	mp, ok := m.procs[pid]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no process %d (has it been stopped?)", pid)
	}
	return mp, nil
}

// remove forgets a stopped process.
func (m *ProcessManager) remove(pid int) {
	m.mu.Lock()
	delete(m.procs, pid)
	m.mu.Unlock()
}

// ProcessStopAll terminates every process the session started. Called when
// the agent shuts down so background REPLs/servers never outlive it.
func ProcessStopAll() {
	processStore.mu.Lock()
	procs := make([]*managedProcess, 0, len(processStore.procs))
	for _, mp := range processStore.procs {
		procs = append(procs, mp)
	}
	processStore.procs = map[int]*managedProcess{}
	processStore.mu.Unlock()
	for _, mp := range procs {
		_, _, _ = mp.stop()
	}
}
