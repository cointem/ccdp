// Package execution is the single local subprocess entry point used by
// tools. It deliberately knows nothing about agent state or permissions;
// callers provide the already-admitted sandbox policy and resource limits.
package execution

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"ccdp/internal/sandbox"
)

// DefaultTimeout is the default bound for a short subprocess invocation.
const DefaultTimeout = 120 * time.Second

// DefaultOutputLimit is the maximum model-visible output retained by a
// command. The process is still drained after the limit is reached so a full
// pipe cannot deadlock the child.
const DefaultOutputLimit = 512 * 1024

// PipeWaitDelay bounds lingering after a shell exits while descendants keep
// inherited stdout/stderr open.
const PipeWaitDelay = 3 * time.Second

// EnvironmentPurpose identifies the owner of a local process environment.
// Every purpose starts from a credential-filtered host environment; callers
// that need an explicit secret must pass that complete environment in
// Request.Env.
type EnvironmentPurpose string

const (
	EnvironmentCommand EnvironmentPurpose = "command"
	EnvironmentGit     EnvironmentPurpose = "git"
	EnvironmentGitHub  EnvironmentPurpose = "github"
	EnvironmentMCP     EnvironmentPurpose = "mcp"
)

// Request describes one admitted local process invocation.
type Request struct {
	Context context.Context
	Command string
	Dir     string
	Timeout time.Duration
	Input   io.Reader
	// Env replaces the inherited environment when non-nil. A nil value gets a
	// credential-filtered copy of the host environment; callers may opt into a
	// narrower purpose-specific set by supplying one explicitly.
	Env []string

	Sandbox *sandbox.Sandbox
	// Progress is the preferred progress path. Run sends each decoded output
	// fragment with a non-blocking channel send. The owner controls the channel
	// lifetime and may cancel the Request.Context; a slow or absent consumer
	// can never stop draining the child process.
	Progress chan<- string
	// NotifyContext is an optional cancellable progress observer. The observer
	// must return when ctx is cancelled. Run joins the observer before it
	// returns, so a compliant observer cannot outlive the invocation or receive
	// progress after the process has been reclaimed.
	NotifyContext func(context.Context, string) error
	// OutputLimit bounds retained output. Zero uses DefaultOutputLimit.
	OutputLimit int
	// Shell is optional; an empty value resolves $SHELL then /bin/sh.
	Shell string
}

// Result is the bounded result of a process invocation.
type Result struct {
	ExitCode  int
	Output    string
	Stdout    string
	Stderr    string
	TimedOut  bool
	Truncated bool
}

// StartRequest describes a long-lived argv process (stdio MCP transport and
// similar adapters). It uses the same interactive/sandbox admission as Run
// but deliberately does not impose Run's short invocation timeout.
type StartRequest struct {
	Context context.Context
	Argv    []string
	Dir     string
	Env     []string
	Sandbox *sandbox.Sandbox
}

// StartArgv starts one admitted process without interpreting argument values
// as shell syntax. Strict macOS sandboxes use the checked profile wrapper;
// ordinary modes retain direct exec argv semantics.
func StartArgv(req StartRequest) (*exec.Cmd, error) {
	if len(req.Argv) == 0 || strings.TrimSpace(req.Argv[0]) == "" {
		return nil, fmt.Errorf("execution: empty argv")
	}
	ctx := req.Context
	if ctx == nil {
		ctx = context.Background()
	}
	parts := make([]string, len(req.Argv))
	for i, arg := range req.Argv {
		parts[i] = shellQuote(arg)
	}
	command := strings.Join(parts, " ")
	if err := sandbox.CheckInteractive(command); err != nil {
		return nil, err
	}
	var cmd *exec.Cmd
	if req.Sandbox != nil && req.Sandbox.CurrentMode() == sandbox.ModeStrict {
		prepared, err := sandbox.PrepareCommandContext(ctx, req.Sandbox, command)
		if err != nil {
			return nil, err
		}
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		cmd = exec.CommandContext(ctx, shell, "-c", prepared)
	} else if req.Sandbox != nil && req.Sandbox.Prefix() != "" {
		// A non-strict sandbox still carries configured per-command limits. Use
		// the shell only for this explicit prefix; argv values remain quoted.
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		cmd = exec.CommandContext(ctx, shell, "-c", req.Sandbox.Prefix()+" "+command)
	} else {
		cmd = exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	}
	cmd.Dir = req.Dir
	if req.Env != nil {
		cmd.Env = append([]string(nil), req.Env...)
	} else {
		cmd.Env = SanitizedEnvironment(os.Environ())
	}
	setProcessGroup(cmd)
	return cmd, nil
}

// SanitizedEnvironment removes common credential-bearing variables before a
// local tool/process inherits the host environment. Provider clients do not
// use this runner; an explicit Request.Env remains the narrow opt-in for a
// process that genuinely needs a secret supplied by its owner.
func SanitizedEnvironment(entries []string) []string {
	return SanitizedEnvironmentFor(EnvironmentCommand, entries)
}

// SanitizedEnvironmentFor is the shared host-environment filter used by
// command, git, checkpoint, and other local-process adapters. The purpose is
// explicit at the boundary so future per-owner policy does not reintroduce
// independent credential filters in each package.
func SanitizedEnvironmentFor(purpose EnvironmentPurpose, entries []string) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		key := entry
		if i := strings.IndexByte(entry, '='); i >= 0 {
			key = entry[:i]
		}
		if secretEnvironmentKey(key) {
			continue
		}
		out = append(out, entry)
	}
	if purpose == EnvironmentGitHub {
		// GitHub CLI is the only local adapter with an explicit token
		// capability. Keep the generic environment filtering above, then opt in
		// to exactly the two token names gh documents; arbitrary *_TOKEN values
		// remain absent from the child environment.
		seen := map[string]bool{}
		for _, entry := range out {
			key := entry
			if i := strings.IndexByte(entry, '='); i >= 0 {
				key = entry[:i]
			}
			seen[key] = true
		}
		for _, entry := range entries {
			key := entry
			if i := strings.IndexByte(entry, '='); i >= 0 {
				key = entry[:i]
			}
			if (key == "GH_TOKEN" || key == "GITHUB_TOKEN") && !seen[key] {
				out = append(out, entry)
				seen[key] = true
			}
		}
	}
	return out
}

func secretEnvironmentKey(key string) bool {
	upper := strings.ToUpper(strings.TrimSpace(key))
	if upper == "" {
		return false
	}
	for _, fragment := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASS", "CREDENTIAL"} {
		if strings.Contains(upper, fragment) {
			return true
		}
	}
	// Common provider/application credentials not covered by the generic
	// fragments above. Keep suffix matching exact so PATH and AUTHORIZATION
	// metadata are not accidentally treated as secrets.
	return upper == "GITHUB_PAT" || strings.HasSuffix(upper, "_PAT") ||
		strings.HasSuffix(upper, "_AUTH") || strings.HasSuffix(upper, "_COOKIE")
}

// Run executes Request through the shell after applying interactive and
// sandbox policy checks. In strict mode Sandbox.PrepareCommand must produce a
// real Darwin profile; an unavailable backend is returned as an error instead
// of silently weakening the policy.
func Run(req Request) (Result, error) {
	if strings.TrimSpace(req.Command) == "" {
		return Result{}, fmt.Errorf("execution: empty command")
	}
	if req.Progress != nil && req.NotifyContext != nil {
		return Result{}, fmt.Errorf("execution: choose one progress sink (Progress or NotifyContext)")
	}
	base := req.Context
	if base == nil {
		base = context.Background()
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	cctx, cancel := context.WithTimeout(base, timeout)
	defer cancel()
	if err := sandbox.CheckInteractive(req.Command); err != nil {
		return Result{}, err
	}
	command, err := sandbox.PrepareCommandContext(cctx, req.Sandbox, req.Command)
	if err != nil {
		return Result{}, err
	}
	shell := req.Shell
	if shell == "" {
		shell = os.Getenv("SHELL")
	}
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.CommandContext(cctx, shell, "-c", command)
	cmd.Dir = req.Dir
	if req.Env != nil {
		cmd.Env = append([]string(nil), req.Env...)
	} else {
		cmd.Env = SanitizedEnvironment(os.Environ())
	}
	if req.Input != nil {
		cmd.Stdin = req.Input
	}
	setProcessGroup(cmd)

	limit := req.OutputLimit
	if limit <= 0 {
		limit = DefaultOutputLimit
	}
	collector := &limitedBuffer{limit: limit}
	var resStdout, resStderr string
	var streamTruncated bool
	var runErr error
	notify := req.NotifyContext
	if req.Progress == nil && notify == nil {
		stdoutCollector := &limitedBuffer{limit: limit}
		stderrCollector := &limitedBuffer{limit: limit}
		cmd.Stdout = stdoutCollector
		cmd.Stderr = stderrCollector
		runErr = cmd.Run()
		terminateProcessGroup(cmd)
		resOutput := stdoutCollector.String()
		if stderrText := stderrCollector.String(); stderrText != "" {
			if resOutput != "" {
				resOutput += "\n"
			}
			resOutput += stderrText
		}
		collector = &limitedBuffer{limit: limit}
		_, _ = collector.Write([]byte(resOutput))
		resStdout, resStderr = stdoutCollector.String(), stderrCollector.String()
		streamTruncated = stdoutCollector.Truncated() || stderrCollector.Truncated()
	} else if req.Progress != nil {
		// A channel is deliberately used instead of an internal callback
		// goroutine. Sending is non-blocking and therefore cannot leak a worker
		// when the owner stops consuming progress.
		pr, pw := io.Pipe()
		cmd.Stdout = pw
		cmd.Stderr = pw
		drainDone := make(chan struct{})
		go func() {
			defer close(drainDone)
			br := bufio.NewReaderSize(pr, 64*1024)
			for {
				fragment, readErr := br.ReadSlice('\n')
				if len(fragment) > 0 {
					_, _ = collector.Write(fragment)
					sendProgress(cctx, req.Progress, strings.TrimSuffix(string(fragment), "\n"))
				}
				if readErr != nil && readErr != bufio.ErrBufferFull {
					return
				}
			}
		}()
		runErr = cmd.Run()
		terminateProcessGroup(cmd)
		_ = pw.Close()
		<-drainDone
	} else {
		pr, pw := io.Pipe()
		cmd.Stdout = pw
		cmd.Stderr = pw
		notifyCh := make(chan string, 64)
		notifyDone := make(chan struct{})
		notifyCtx, notifyCancel := context.WithCancel(cctx)
		go func() {
			defer close(notifyDone)
			for {
				// Cancellation takes priority over queued progress. This prevents an
				// observer from receiving stale output after the process has ended.
				select {
				case <-notifyCtx.Done():
					return
				default:
				}
				select {
				case <-notifyCtx.Done():
					return
				case line, ok := <-notifyCh:
					if !ok {
						return
					}
					if err := notify(notifyCtx, line); err != nil && notifyCtx.Err() != nil {
						return
					}
				}
			}
		}()
		drainDone := make(chan struct{})
		go func() {
			defer close(drainDone)
			br := bufio.NewReaderSize(pr, 64*1024)
			for {
				fragment, readErr := br.ReadSlice('\n')
				if len(fragment) > 0 {
					_, _ = collector.Write(fragment)
					line := strings.TrimSuffix(string(fragment), "\n")
					// Progress is best effort. The bounded queue prevents a slow UI
					// callback from blocking pipe draining or process cancellation.
					// Once cancellation begins, stop producing notifications but keep
					// reading until EOF. Returning here would leave exec's stdout
					// copier blocked in io.PipeWriter.Write while cmd.Wait tries to
					// reclaim the child.
					if notifyCtx.Err() == nil {
						select {
						case notifyCh <- line:
						default:
						}
					}
				}
				if readErr != nil {
					if readErr != bufio.ErrBufferFull {
						// EOF or another read error ends draining. The process result
						// still contains all bytes consumed before this point.
						break
					}
				}
			}
		}()
		runErr = cmd.Run()
		terminateProcessGroup(cmd)
		notifyCancel()
		_ = pw.Close()
		<-drainDone
		close(notifyCh)
		// NotifyContext has an explicit cancellation contract. Joining here is
		// intentional: no observer goroutine can remain after Run returns.
		<-notifyDone
	}

	res := Result{Output: collector.String(), Stdout: resStdout, Stderr: resStderr, Truncated: collector.Truncated() || streamTruncated}
	if errors.Is(runErr, exec.ErrWaitDelay) {
		// A shell can exit while a background descendant keeps stdout/stderr
		// open. WaitDelay prevents an unbounded wait; the process group cleanup
		// above then reclaims that descendant instead of leaving it orphaned.
		res.Truncated = true
		res.Output += "\n[command output pipe closed after descendant cleanup]"
	}
	// A process can finish successfully at the same instant the wrapper's
	// deadline fires. A canceled context alone is not proof that the command
	// timed out; only classify it as timed out when the command itself returned
	// an error (the process was still running when cancellation was observed).
	processFailed := runErr != nil && !benignPipeClose(runErr) && !errors.Is(runErr, exec.ErrWaitDelay)
	if cctx.Err() == context.DeadlineExceeded && processFailed {
		res.TimedOut = true
		res.ExitCode = -1
		res.Output += fmt.Sprintf("\n[command timed out after %s]", timeout)
		return res, nil
	}
	if runErr == nil || benignPipeClose(runErr) || errors.Is(runErr, exec.ErrWaitDelay) {
		return res, nil
	}
	if ee, ok := runErr.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	return res, fmt.Errorf("execution: %w", runErr)
}

// RunArgv executes argv without interpreting caller-provided values as shell
// syntax. It still enters the same sandbox runner; in strict mode argv is
// safely shell-quoted only to place it inside the Darwin profile's /bin/sh.
func RunArgv(ctx context.Context, argv []string, req Request) (Result, error) {
	if len(argv) == 0 || argv[0] == "" {
		return Result{}, fmt.Errorf("execution: empty argv")
	}
	parts := make([]string, len(argv))
	for i, arg := range argv {
		parts[i] = shellQuote(arg)
	}
	req.Command = strings.Join(parts, " ")
	if req.Context == nil {
		req.Context = ctx
	}
	return Run(req)
}

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
	cmd.WaitDelay = PipeWaitDelay
}

// terminateProcessGroup reclaims descendants that inherited the shell's
// stdout/stderr. CommandContext kills the group on cancellation, but a
// normal shell exit does not invoke cmd.Cancel; without this explicit cleanup
// a background child could survive a short command indefinitely.
func terminateProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		// The command result remains authoritative; cleanup is best effort after
		// Wait has already completed.
		return
	}
}

func sendProgress(ctx context.Context, progress chan<- string, line string) {
	if progress == nil {
		return
	}
	select {
	case <-ctx.Done():
		return
	default:
	}
	select {
	case <-ctx.Done():
	case progress <- line:
	default:
	}
}

func benignPipeClose(err error) bool {
	return errors.Is(err, exec.ErrWaitDelay) || errors.Is(err, os.ErrClosed)
}

type limitedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit <= 0 {
		return len(p), nil
	}
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		_, _ = b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	_, _ = b.buf.Write(p)
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *limitedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// QuoteArg returns a shell-safe representation for an argument that will be
// passed through the shared shell runner. It is exported for fixed-argv tools
// such as Git; callers should still run the unquoted command through sandbox
// policy before constructing the quoted form.
func QuoteArg(s string) string { return shellQuote(s) }
