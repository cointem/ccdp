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

var startScratchCleanup sync.Map // map[*exec.Cmd]func()

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
	Argv    []string
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
// as shell syntax. The process and its descendants inherit Seatbelt.
func StartArgv(req StartRequest) (*exec.Cmd, error) {
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
	if err := sandbox.CheckInteractive(strings.Join(req.Argv, " ")); err != nil {
		return nil, err
	}
	policy := req.Sandbox.Snapshot()
	procEnv, cleanupScratch, err := prepareProcessEnvironment(req.Env, policy)
	if err != nil {
		return nil, err
	}
	argv := append([]string(nil), req.Argv...)
	argv, procEnv = preferNativeGit(argv, "", procEnv, req.Dir)
	argv, procEnv = preferNativeCLTPython(argv, "", procEnv, req.Dir, nativeCLTPythonPath())
	addExecutionReadRoots(policy, argv, "", req.Dir, procEnv)
	profile, err := policy.Profile()
	if err != nil {
		cleanupScratch()
		return nil, err
	}
	if err := verifyProfileContext(ctx, profile); err != nil {
		cleanupScratch()
		return nil, err
	}
	argv = applyArgvLimits(policy, argv)
	cmd := exec.CommandContext(ctx, sandbox.BackendPath, append([]string{"-p", profile, "--"}, argv...)...)
	cmd.Dir = req.Dir
	cmd.Env = procEnv
	setProcessGroup(cmd)
	if cleanupScratch != nil {
		startScratchCleanup.Store(cmd, cleanupScratch)
	}
	return cmd, nil
}

// StartShell creates a fixed /bin/sh invocation inside Seatbelt for a
// long-running shell command. The owner remains responsible for process pipes
// and lifetime.
func StartShell(ctx context.Context, command, dir string, env []string, policy *sandbox.Sandbox) (*exec.Cmd, error) {
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
	_, procEnv = preferNativeGit(nil, command, procEnv, dir)
	_, procEnv = preferNativeCLTPython(nil, command, procEnv, dir, nativeCLTPythonPath())
	addExecutionReadRoots(policy, nil, command, dir, procEnv)
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
	cmd := exec.CommandContext(ctx, sandbox.BackendPath, "-p", profile, "--", "/bin/sh", "-c", prepared)
	cmd.Dir = dir
	cmd.Env = procEnv
	setProcessGroup(cmd)
	if cleanupScratch != nil {
		startScratchCleanup.Store(cmd, cleanupScratch)
	}
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
	cctx, cancel := context.WithTimeout(base, timeout)
	defer cancel()
	if req.Sandbox == nil {
		return Result{}, fmt.Errorf("execution: sandbox policy is unavailable")
	}
	policy := req.Sandbox.Snapshot()
	procEnv, cleanupScratch, err := prepareProcessEnvironment(req.Env, policy)
	if err != nil {
		return Result{}, err
	}
	argv, procEnv := preferNativeGit(req.Argv, req.Command, procEnv, req.Dir)
	argv, procEnv = preferNativeCLTPython(argv, req.Command, procEnv, req.Dir, nativeCLTPythonPath())
	defer cleanupScratch()
	addExecutionReadRoots(policy, argv, req.Command, req.Dir, procEnv)
	var cmd *exec.Cmd
	if len(req.Argv) > 0 {
		if argv[0] == "" {
			return Result{}, fmt.Errorf("execution: empty argv")
		}
		if err := sandbox.CheckInteractive(strings.Join(argv, " ")); err != nil {
			return Result{}, err
		}
		profile, err := policy.Profile()
		if err != nil {
			return Result{}, err
		}
		if err := verifyProfileContext(cctx, profile); err != nil {
			return Result{}, err
		}
		limitedArgv := applyArgvLimits(policy, argv)
		cmd = exec.CommandContext(cctx, sandbox.BackendPath, append([]string{"-p", profile, "--"}, limitedArgv...)...)
	} else {
		command, err := sandbox.PrepareCommandContext(cctx, policy, req.Command)
		if err != nil {
			return Result{}, err
		}
		profile, err := policy.Profile()
		if err != nil {
			return Result{}, err
		}
		cmd = exec.CommandContext(cctx, sandbox.BackendPath, "-p", profile, "--", "/bin/sh", "-c", command)
	}
	cmd.Dir = req.Dir
	cmd.Env = procEnv
	if req.Input != nil {
		cmd.Stdin = req.Input
	}
	setProcessGroup(cmd)
	if err := cctx.Err(); err == nil {
		policy.MarkExternalExecution()
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

func prepareProcessEnvironment(env []string, policy *sandbox.Sandbox) ([]string, func(), error) {
	if policy == nil {
		return nil, nil, fmt.Errorf("execution: sandbox policy is unavailable")
	}
	entries := append([]string(nil), env...)
	if env == nil {
		entries = SanitizedEnvironment(os.Environ())
	} else {
		entries = filterHostDelegationEnvironment(entries)
	}
	var scratch string
	for _, root := range policy.Snapshot().ScratchDirs {
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			scratch = root
			break
		}
	}
	var cleanup func()
	if scratch == "" {
		var err error
		scratch, err = os.MkdirTemp("", "ccdp-sandbox-")
		if err != nil {
			return nil, nil, fmt.Errorf("execution: create invocation scratch directory: %w", err)
		}
		policy.AddExecutionScratchDir(scratch)
		cleanup = func() { _ = os.RemoveAll(scratch) }
	}
	cache := filepath.Join(scratch, "cache")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, nil, fmt.Errorf("execution: initialize invocation cache: %w", err)
	}
	entries = setEnvironment(entries, "HOME", scratch)
	entries = setEnvironment(entries, "TMPDIR", scratch)
	entries = setEnvironment(entries, "TMP", scratch)
	entries = setEnvironment(entries, "TEMP", scratch)
	entries = setEnvironment(entries, "XDG_CACHE_HOME", cache)
	entries = setEnvironment(entries, "GOCACHE", filepath.Join(cache, "go-build"))
	entries = setEnvironment(entries, "npm_config_cache", filepath.Join(cache, "npm"))
	entries = setEnvironment(entries, "PIP_CACHE_DIR", filepath.Join(cache, "pip"))
	if cleanup == nil {
		cleanup = func() {}
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

const nativeGitPath = "/Library/Developer/CommandLineTools/usr/bin/git"

// preferNativeGit avoids Apple's /usr/bin/git xcrun launcher when it is the
// selected executable. xcrun creates a database under the host's
// DARWIN_USER_TEMP_DIR (which ignores TMPDIR), so allowing that host directory
// would expose unrelated user temporary data. The actual selected CLT binary
// is already covered by the narrow developer-toolchain read root.
func preferNativeGit(argv []string, command string, env []string, dir string) ([]string, []string) {
	if executableFile(nativeGitPath) == "" {
		return argv, env
	}
	path := environmentValue(env, "PATH")
	if path == "" {
		path = os.Getenv("PATH")
	}
	if len(argv) > 0 {
		resolved := resolveExecutable(argv[0], path, dir)
		if resolved == "/usr/bin/git" {
			argv = append([]string(nil), argv...)
			argv[0] = nativeGitPath
		}
		return argv, env
	}
	for _, name := range shellCommandNames(command) {
		if filepath.Base(name) == "git" && resolveExecutable(name, path, dir) == "/usr/bin/git" {
			return argv, prependPath(env, filepath.Dir(nativeGitPath), path)
		}
	}
	return argv, env
}

const nativeCLTPythonShimPath = "/Library/Developer/CommandLineTools/usr/bin/python3"

// preferNativeCLTPython bypasses Apple's /usr/bin/python3 launcher when it
// resolves to the Command Line Tools shim. That launcher asks xcrun to locate
// the runtime and writes an xcrun database under DARWIN_USER_TEMP_DIR, outside
// the per-invocation TMPDIR. The direct framework interpreter avoids that host
// temporary state; addExecutionReadRoots then authorizes only its versioned
// Python.framework runtime root.
func preferNativeCLTPython(argv []string, command string, env []string, dir, nativePath string) ([]string, []string) {
	if nativePath == "" {
		return argv, env
	}
	nativePath = executableFile(nativePath)
	if nativePath == "" || pythonFrameworkVersionRoot(nativePath) == "" {
		return argv, env
	}
	path := environmentValue(env, "PATH")
	if path == "" {
		path = os.Getenv("PATH")
	}
	if len(argv) > 0 {
		resolved := resolveExecutable(argv[0], path, dir)
		if isSystemPython3Shim(resolved) && nativePythonSupportsName(filepath.Base(argv[0]), nativePath) {
			argv = append([]string(nil), argv...)
			argv[0] = nativePath
		}
		return argv, env
	}
	for _, name := range shellCommandNames(command) {
		resolved := resolveExecutable(name, path, dir)
		if isSystemPython3Shim(resolved) && nativePythonSupportsName(filepath.Base(name), nativePath) {
			return argv, prependPath(env, filepath.Dir(nativePath), path)
		}
	}
	return argv, env
}

func nativeCLTPythonPath() string {
	path := executableFile(nativeCLTPythonShimPath)
	if path == "" || pythonFrameworkVersionRoot(path) == "" {
		return ""
	}
	return path
}

func isSystemPython3Shim(path string) bool {
	if filepath.Dir(filepath.Clean(path)) != "/usr/bin" {
		return false
	}
	name := filepath.Base(path)
	if name == "python3" {
		return true
	}
	if !strings.HasPrefix(name, "python3.") {
		return false
	}
	for _, digit := range strings.TrimPrefix(name, "python3.") {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return len(name) > len("python3.")
}

func nativePythonSupportsName(name, nativePath string) bool {
	return name == "python3" || filepath.Base(name) == filepath.Base(nativePath)
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

func prependPath(env []string, first, existing string) []string {
	var entries []string
	for _, entry := range filepath.SplitList(existing) {
		if entry != first && entry != "" {
			entries = append(entries, entry)
		}
	}
	entries = append([]string{first}, entries...)
	return setEnvironment(env, "PATH", strings.Join(entries, string(os.PathListSeparator)))
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

// CleanupStartedProcess releases an invocation scratch directory after a
// long-lived command has exited. Callers that start a command must call this
// after Wait, even when Wait reports an error.
func CleanupStartedProcess(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if value, ok := startScratchCleanup.LoadAndDelete(cmd); ok {
		value.(func())()
	}
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

func addExecutionReadRoots(policy *sandbox.Sandbox, argv []string, command, dir string, env []string) {
	if policy == nil {
		return
	}
	searchPath := os.Getenv("PATH")
	for _, entry := range env {
		if strings.HasPrefix(entry, "PATH=") {
			searchPath = strings.TrimPrefix(entry, "PATH=")
			break
		}
	}
	var names []string
	if len(argv) > 0 {
		names = append(names, argv[0])
	} else {
		names = shellCommandNames(command)
	}
	for _, name := range names {
		path := resolveExecutable(name, searchPath, dir)
		if path == "" {
			continue
		}
		if root := executableReadRoot(path, policy.Workspace); root != "" {
			policy.AddExecutionReadRoot(root)
		}
	}
}

func resolveExecutable(name, searchPath, dir string) string {
	if name == "" {
		return ""
	}
	if strings.ContainsRune(name, filepath.Separator) {
		if !filepath.IsAbs(name) {
			if dir == "" {
				dir, _ = os.Getwd()
			}
			name = filepath.Join(dir, name)
		}
		return executableFile(name)
	}
	for _, entry := range filepath.SplitList(searchPath) {
		if entry == "" {
			entry = "."
		}
		candidate := filepath.Join(entry, name)
		if resolved := executableFile(candidate); resolved != "" {
			return resolved
		}
	}
	return ""
}

func executableFile(path string) string {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Mode()&0111 == 0 {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	return filepath.Clean(path)
}

func executableReadRoot(executable, workspace string) string {
	executable = filepath.Clean(executable)
	if workspace != "" && withinPath(workspace, executable) {
		return ""
	}
	for _, root := range []string{"/bin", "/usr/bin", "/sbin", "/usr/sbin", "/usr/lib", "/System"} {
		if withinPath(root, executable) {
			return ""
		}
	}
	for _, root := range []string{"/usr/local/go", "/Library/Developer/CommandLineTools/usr"} {
		if withinPath(root, executable) {
			return root
		}
	}
	if root := pythonFrameworkVersionRoot(executable); root != "" {
		return root
	}
	parts := strings.Split(filepath.ToSlash(executable), "/")
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] == "Cellar" && parts[i+1] != "" && parts[i+2] != "" {
			return filepath.FromSlash(strings.Join(parts[:i+3], "/"))
		}
	}
	for i := 0; i+3 < len(parts); i++ {
		if parts[i] == ".nvm" && parts[i+1] == "versions" && parts[i+2] == "node" {
			return filepath.FromSlash(strings.Join(parts[:i+4], "/"))
		}
		if parts[i] == ".pyenv" && parts[i+1] == "versions" {
			return filepath.FromSlash(strings.Join(parts[:i+3], "/"))
		}
		if parts[i] == ".rustup" && parts[i+1] == "toolchains" {
			return filepath.FromSlash(strings.Join(parts[:i+3], "/"))
		}
	}
	for _, root := range []string{
		"/Applications/Xcode.app/Contents/Developer/Toolchains/XcodeDefault.xctoolchain",
		"/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs",
	} {
		if withinPath(root, executable) {
			return root
		}
	}
	// For an unfamiliar user-installed executable, grant only that binary's
	// read/execute path. Its containing directory may hold unrelated user data.
	return executable
}

func pythonFrameworkVersionRoot(executable string) string {
	parts := strings.Split(filepath.ToSlash(filepath.Clean(executable)), "/")
	for i := 0; i+3 < len(parts); i++ {
		if parts[i] == "Python3.framework" && parts[i+1] == "Versions" && parts[i+2] != "" {
			return filepath.FromSlash(strings.Join(parts[:i+3], "/"))
		}
	}
	return ""
}

func withinPath(root, child string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(child))
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

func shellCommandNames(command string) []string {
	var segments []string
	var b strings.Builder
	quote := rune(0)
	escaped := false
	flush := func() {
		if segment := strings.TrimSpace(b.String()); segment != "" {
			segments = append(segments, segment)
		}
		b.Reset()
	}
	for _, r := range command {
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			b.WriteRune(r)
			escaped = true
			continue
		}
		if quote != 0 {
			b.WriteRune(r)
			if r == quote {
				quote = 0
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			b.WriteRune(r)
			continue
		}
		if r == ';' || r == '&' || r == '|' || r == '\n' || r == '(' || r == ')' {
			flush()
			continue
		}
		b.WriteRune(r)
	}
	flush()

	var names []string
	for _, segment := range segments {
		words := shellWords(segment)
		for i := 0; i < len(words); i++ {
			word := words[i]
			if word == "" || strings.Contains(word, "=") && !strings.Contains(word, "/") && !strings.HasPrefix(word, "=") {
				continue
			}
			if shellKeywordOrBuiltin(word) {
				if word == "command" || word == "exec" || word == "builtin" {
					continue
				}
				if word == "then" || word == "else" || word == "do" || word == "!" {
					continue
				}
				break
			}
			names = append(names, word)
			if word == "env" || word == "nice" || word == "nohup" || word == "time" || word == "timeout" {
				continue
			}
			break
		}
	}
	return names
}

func shellKeywordOrBuiltin(word string) bool {
	switch word {
	case "if", "then", "else", "elif", "fi", "for", "while", "until", "do", "done", "case", "esac", "in", "function", "!",
		"cd", "echo", "printf", "export", "readonly", "local", "set", "unset", "shift", "read", "test", "[", ":", "true", "false", "ulimit", "umask", "wait", "jobs", "break", "continue", "return", "exit", "source", ".":
		return true
	default:
		return false
	}
}

func shellWords(segment string) []string {
	var words []string
	var b strings.Builder
	quote := rune(0)
	escaped := false
	active := false
	flush := func() {
		if active {
			words = append(words, b.String())
			b.Reset()
			active = false
		}
	}
	for _, r := range segment {
		if escaped {
			b.WriteRune(r)
			active = true
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			active = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				b.WriteRune(r)
			}
			active = true
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			active = true
			continue
		}
		if r == ' ' || r == '\t' || r == '\r' {
			flush()
			continue
		}
		b.WriteRune(r)
		active = true
	}
	flush()
	return words
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
	if ctx == nil {
		ctx = context.Background()
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	probe := exec.CommandContext(probeCtx, sandbox.BackendPath, "-p", profile, "--", "/bin/sh", "-c", "true")
	if output, err := probe.CombinedOutput(); err != nil {
		return fmt.Errorf("execution: Seatbelt rejected the profile: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
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
