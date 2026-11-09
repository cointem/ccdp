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
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ccdp/internal/netguard"
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
	readOnlyQuery bool      // set only by typed read-only adapters
	readOnlyRoot  string    // strict snapshot data boundary for typed search
	stdoutSink    io.Writer // optional bounded machine-output parser owned by a typed adapter
	Context       context.Context
	Command       string
	Argv          []string
	Dir           string
	Timeout       time.Duration
	Input         io.Reader
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
	readOnlyQuery bool
	Context       context.Context
	Argv          []string
	Dir           string
	Env           []string
	Sandbox       *sandbox.Sandbox
}

// StartArgv starts one admitted process without interpreting argument values
// as shell syntax. The process and its descendants inherit Seatbelt.
func StartArgv(req StartRequest) (*Process, error) {
	if len(req.Argv) == 0 || strings.TrimSpace(req.Argv[0]) == "" {
		return nil, fmt.Errorf("execution: empty argv")
	}
	ctx := req.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if req.Sandbox == nil {
		return nil, fmt.Errorf("execution: sandbox policy is unavailable")
	}
	if err := sandbox.CheckInteractiveArgv(req.Argv); err != nil {
		return nil, err
	}
	policy := req.Sandbox.Snapshot()
	if req.readOnlyQuery {
		policy.AllowNetwork = false
		policy.NetworkProxy = ""
	}
	procEnv, cleanupScratch, err := prepareProcessEnvironment(req.Env, policy)
	if err != nil {
		return nil, err
	}
	argv := append([]string(nil), req.Argv...)
	profile, err := processProfile(policy, argv, req.readOnlyQuery)
	if err != nil {
		cleanupScratch()
		return nil, err
	}
	if err := verifyProcessProfile(ctx, profile, argv, req.readOnlyQuery); err != nil {
		cleanupScratch()
		return nil, err
	}
	if !req.readOnlyQuery {
		argv = applyArgvLimits(policy, argv)
	}
	cmd := newProcess(exec.CommandContext(ctx, sandbox.BackendPath, append([]string{"-p", profile, "--"}, argv...)...))
	cmd.cmd.Dir = req.Dir
	cmd.cmd.Env = procEnv
	cmd.cleanup = cleanupScratch
	if !req.readOnlyQuery {
		cmd.executionPolicy = policy
	}
	return bindController(cmd, ctx, policy), nil
}

// StartShell creates a fixed /bin/sh invocation inside Seatbelt for a
// long-running shell command. The owner remains responsible for process pipes
// and lifetime.
func StartShell(ctx context.Context, command, dir string, env []string, policy *sandbox.Sandbox) (*Process, error) {
	if strings.TrimSpace(command) == "" {
		return nil, fmt.Errorf("execution: empty command")
	}
	if policy == nil {
		return nil, fmt.Errorf("execution: sandbox policy is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	policy = policy.Snapshot()
	procEnv, cleanupScratch, err := prepareProcessEnvironment(env, policy)
	if err != nil {
		return nil, err
	}
	prepared, err := sandbox.PrepareCommandContext(ctx, policy, command)
	if err != nil {
		cleanupScratch()
		return nil, err
	}
	profile, err := policy.Profile()
	if err != nil {
		cleanupScratch()
		return nil, err
	}
	cmd := newProcess(exec.CommandContext(ctx, sandbox.BackendPath, "-p", profile, "--", "/bin/sh", "-c", prepared))
	cmd.cmd.Dir = dir
	cmd.cmd.Env = procEnv
	cmd.cleanup = cleanupScratch
	cmd.executionPolicy = policy
	return bindController(cmd, ctx, policy), nil
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
	switch upper {
	case "SSH_AUTH_SOCK", "GPG_AGENT_INFO", "DBUS_SESSION_BUS_ADDRESS", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
		return true
	}
	if strings.HasSuffix(upper, "_PROXY") {
		return true
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
	if len(req.Argv) == 0 && strings.TrimSpace(req.Command) == "" {
		return Result{}, fmt.Errorf("execution: empty command")
	}
	if len(req.Argv) > 0 && req.Command != "" {
		return Result{}, fmt.Errorf("execution: choose Command or Argv")
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
	// Preparation has its own bounded profile probes. The command's timeout
	// starts only when the process is dispatched, after environment setup.
	cctx, cancel := context.WithCancel(base)
	defer cancel()
	if req.Sandbox == nil {
		return Result{}, fmt.Errorf("execution: sandbox policy is unavailable")
	}
	policy := req.Sandbox.Snapshot()
	if req.readOnlyQuery {
		policy.AllowNetwork = false
		policy.NetworkProxy = ""
	}
	procEnv, cleanupScratch, err := prepareProcessEnvironment(req.Env, policy)
	if err != nil {
		return Result{}, err
	}
	argv := append([]string(nil), req.Argv...)
	defer cleanupScratch()
	var cmd *Process
	if len(req.Argv) > 0 {
		if argv[0] == "" {
			return Result{}, fmt.Errorf("execution: empty argv")
		}
		if err := sandbox.CheckInteractiveArgv(argv); err != nil {
			return Result{}, err
		}
		profile, err := processProfile(policy, argv, req.readOnlyQuery, req.readOnlyRoot)
		if err != nil {
			return Result{}, err
		}
		if err := verifyProcessProfile(cctx, profile, argv, req.readOnlyQuery); err != nil {
			if cctx.Err() != nil {
				return Result{TimedOut: true, ExitCode: -1}, nil
			}
			return Result{}, err
		}
		limitedArgv := argv
		if !req.readOnlyQuery {
			limitedArgv = applyArgvLimits(policy, argv)
		}
		cmd = newProcess(exec.CommandContext(cctx, sandbox.BackendPath, append([]string{"-p", profile, "--"}, limitedArgv...)...))
	} else {
		command, err := sandbox.PrepareCommandContext(cctx, policy, req.Command)
		if err != nil {
			if cctx.Err() != nil {
				return Result{TimedOut: true, ExitCode: -1}, nil
			}
			return Result{}, err
		}
		profile, err := policy.Profile()
		if err != nil {
			return Result{}, err
		}
		cmd = newProcess(exec.CommandContext(cctx, sandbox.BackendPath, "-p", profile, "--", "/bin/sh", "-c", command))
	}
	cmd = bindController(cmd, cctx, policy)
	cmd.SetTimeout(timeout)
	cmd.cmd.Dir = req.Dir
	cmd.cmd.Env = procEnv
	if req.Input != nil {
		cmd.cmd.Stdin = req.Input
	}
	if !req.readOnlyQuery {
		cmd.executionPolicy = policy
	}

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
		cmd.cmd.Stdout = stdoutCollector
		if req.stdoutSink != nil {
			cmd.cmd.Stdout = io.MultiWriter(stdoutCollector, req.stdoutSink)
		}
		cmd.cmd.Stderr = stderrCollector
		runErr = cmd.Run()
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
		cmd.cmd.Stdout = pw
		cmd.cmd.Stderr = pw
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
		_ = pw.Close()
		<-drainDone
	} else {
		pr, pw := io.Pipe()
		cmd.cmd.Stdout = pw
		cmd.cmd.Stderr = pw
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
	if (cmd.Reason() == "timed_out" || cctx.Err() == context.DeadlineExceeded) && processFailed {
		res.TimedOut = true
		res.ExitCode = -1
		res.Output += fmt.Sprintf("\n[command timed out after %s]", timeout)
		return res, nil
	}
	if runErr == nil || benignPipeClose(runErr) || errors.Is(runErr, exec.ErrWaitDelay) {
		return res, nil
	}
	if ee, ok := runErr.(interface{ ExitCode() int }); ok {
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	return res, fmt.Errorf("execution: %w", runErr)
}

// Preparation never dispatches the requested command. A failure affects this
// attempt only; a later invocation can retry after its filesystem/config is fixed.
func prepareProcessEnvironment(env []string, policy *sandbox.Sandbox) ([]string, func(), error) {
	if policy == nil {
		return nil, nil, fmt.Errorf("execution: sandbox policy is unavailable")
	}
	if err := CheckEnvironment(policy); err != nil {
		return nil, nil, err
	}
	controller := controllerFor(policy.ExecutorID)
	if controller == nil {
		return nil, nil, ErrEnvironmentUnavailable
	}
	if err := controller.Protect(policy); err != nil {
		return nil, nil, err
	}
	entries := append([]string(nil), env...)
	if env == nil {
		entries = SanitizedEnvironment(os.Environ())
	} else {
		entries = filterHostDelegationEnvironment(entries)
	}
	var scratch string
	for _, root := range policy.Snapshot().ScratchDirs {
		// A missing owned scratch is repaired once, before dispatch, using
		// the same stable path. Never substitute a throwaway HOME/cache.
		if err := os.Mkdir(root, 0700); err != nil && !os.IsExist(err) {
			return nil, nil, fmt.Errorf("%w: prepare owned scratch: %v", ErrEnvironmentUnavailable, err)
		}
		scratch = root
		break
	}
	if scratch == "" {
		return nil, nil, fmt.Errorf("%w: missing session scratch directory", ErrEnvironmentUnavailable)
	}
	cleanup := func() {}
	cache, err := prepareDevelopmentCache(policy)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: initialize project cache: %v", ErrEnvironmentUnavailable, err)
	}
	if environmentValue(entries, "HOME") == "" {
		if home, err := os.UserHomeDir(); err == nil {
			entries = setEnvironment(entries, "HOME", home)
		}
	}
	entries = setEnvironment(entries, "TMPDIR", scratch)
	entries = setEnvironment(entries, "TMP", scratch)
	entries = setEnvironment(entries, "TEMP", scratch)
	entries = setEnvironment(entries, "XDG_RUNTIME_DIR", scratch)
	entries = setEnvironment(entries, "XDG_CACHE_HOME", cache)
	entries = setEnvironment(entries, "GOCACHE", filepath.Join(cache, "go-build"))
	entries = setEnvironment(entries, "GOMODCACHE", filepath.Join(cache, "go-mod"))
	entries = setEnvironment(entries, "GOTMPDIR", scratch)
	entries = setEnvironment(entries, "npm_config_cache", filepath.Join(cache, "npm"))
	entries = setEnvironment(entries, "PIP_CACHE_DIR", filepath.Join(cache, "pip"))
	entries = setEnvironment(entries, "UV_CACHE_DIR", filepath.Join(cache, "uv"))
	entries = setEnvironment(entries, "npm_config_store_dir", filepath.Join(cache, "pnpm"))
	entries = setEnvironment(entries, "CARGO_HOME", filepath.Join(cache, "cargo"))
	if environmentValue(entries, "RUSTUP_HOME") == "" {
		entries = setEnvironment(entries, "RUSTUP_HOME", filepath.Join(environmentValue(entries, "HOME"), ".rustup"))
	}
	entries, err = prepareToolConfiguration(entries, policy)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	if policy.AllowNetwork && policy.NetworkProxy != "" {
		gateway, err := netguard.StartGateway(policy.NetworkProxy)
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		scratchCleanup := cleanup
		cleanup = func() { gateway.Close(); scratchCleanup() }
		policy.AllowNetwork = false
		if err := policy.AddLocalService(sandbox.LocalService{Direction: "connect", Protocol: "tcp", Port: uint16(gateway.Port)}); err != nil {
			cleanup()
			return nil, nil, err
		}
		for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
			entries = setEnvironment(entries, key, gateway.URL)
		}
		entries = setEnvironment(entries, "NO_PROXY", "")
		entries = setEnvironment(entries, "no_proxy", "")
	}
	return entries, cleanup, nil
}

// filterHostDelegationEnvironment removes proxy and agent sockets even when a
// caller supplies an explicit environment. Explicit credentials remain
// available for purpose-specific adapters such as MCP/gh, but cannot smuggle
// host network or authentication services into a sandboxed child.
func filterHostDelegationEnvironment(entries []string) []string {
	filtered := make([]string, 0, len(entries))
	for _, entry := range entries {
		key := entry
		if i := strings.IndexByte(entry, '='); i >= 0 {
			key = entry[:i]
		}
		upper := strings.ToUpper(strings.TrimSpace(key))
		if upper == "SSH_AUTH_SOCK" || upper == "GPG_AGENT_INFO" || upper == "DBUS_SESSION_BUS_ADDRESS" || upper == "HTTP_PROXY" || upper == "HTTPS_PROXY" || upper == "ALL_PROXY" || upper == "NO_PROXY" || strings.HasSuffix(upper, "_PROXY") {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func environmentValue(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

func setEnvironment(entries []string, key, value string) []string {
	filtered := entries[:0]
	prefix := key + "="
	for _, entry := range entries {
		if !strings.HasPrefix(entry, prefix) {
			filtered = append(filtered, entry)
		}
	}
	return append(filtered, prefix+value)
}

// CleanupStartedProcess releases a prepared process abandoned before Start.
// Started processes call it themselves after reaping; repeated calls are safe.
func CleanupStartedProcess(cmd *Process) {
	if cmd == nil {
		return
	}
	cmd.cleanupOnce.Do(func() {
		cmd.mu.Lock()
		started := cmd.started
		cmd.mu.Unlock()
		if !started {
			if cmd.stdinRead != nil {
				_ = cmd.stdinRead.Close()
			}
			if cmd.stdoutRead != nil {
				_ = cmd.stdoutRead.Close()
			}
			if cmd.stdoutWrite != nil {
				_ = cmd.stdoutWrite.Close()
			}
			if cmd.input != nil {
				_ = cmd.input.Close()
			}
			if cmd.slave != nil {
				_ = cmd.slave.Close()
			}
		}
		if cmd.cleanup != nil {
			cmd.cleanup()
		}
	})
}

func applyArgvLimits(policy *sandbox.Sandbox, argv []string) []string {
	if policy == nil {
		return append([]string(nil), argv...)
	}
	prefix := strings.TrimSpace(policy.Prefix())
	if prefix == "" {
		return append([]string(nil), argv...)
	}
	prefix = strings.TrimSuffix(prefix, "&&")
	command := strings.TrimSpace(prefix) + ` && exec "$@"`
	wrapped := []string{"/bin/sh", "-c", command, "ccdp-argv"}
	return append(wrapped, argv...)
}

func withinPath(root, child string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(child))
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

// RunArgv executes argv without interpreting caller-provided values as shell
// syntax. The fixed Seatbelt binary directly launches argv.
func RunArgv(ctx context.Context, argv []string, req Request) (Result, error) {
	if len(argv) == 0 || argv[0] == "" {
		return Result{}, fmt.Errorf("execution: empty argv")
	}
	req.Argv = append([]string(nil), argv...)
	req.Command = ""
	if req.Context == nil {
		req.Context = ctx
	}
	return Run(req)
}

func verifyProfileContext(ctx context.Context, profile string) error {
	return sandbox.VerifyProfile(ctx, profile, "/bin/sh", "-c", "true")
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
