package execution

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"ccdp/internal/sandbox"
)

func TestRunNotifyDrainsUnterminatedLargeLine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	res, err := Run(testRequest(t, Request{
		Context:     ctx,
		Command:     "head -c 2097152 /dev/zero",
		OutputLimit: 64 * 1024,
		Progress:    make(chan string),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("large unterminated output took too long: %s", time.Since(start))
	}
	if !res.Truncated || len(res.Output) != 64*1024 {
		t.Fatalf("bounded result = len %d truncated %v", len(res.Output), res.Truncated)
	}
}

func TestRunSlowNotifyDoesNotBlockProcessDrain(t *testing.T) {
	start := time.Now()
	res, err := Run(testRequest(t, Request{
		Context:     context.Background(),
		Command:     "for i in $(seq 1 100); do echo line; done",
		OutputLimit: 1024,
		NotifyContext: func(context.Context, string) error {
			time.Sleep(500 * time.Millisecond)
			return nil
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("slow notify blocked completion: %s", time.Since(start))
	}
	if !strings.Contains(res.Output, "line") {
		t.Fatalf("missing drained output: %q", res.Output)
	}
}

func TestRunCancellationKillsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	res, err := Run(testRequest(t, Request{Context: ctx, Command: "sleep 10"}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.ExitCode != -1 {
		t.Fatalf("cancellation result = %+v", res)
	}
}

func TestRunSuccessfulExitNearDeadlineIsNotClassifiedAsTimeout(t *testing.T) {
	// The command exits successfully before its wrapper deadline, while the
	// cancellable progress observer deliberately takes longer to clean up. The
	// wrapper context may expire during that cleanup, but that is not a process
	// timeout and must not turn an exit-0 result into TimedOut.
	res, err := Run(testRequest(t, Request{
		Context: context.Background(),
		Command: "printf 'done\\n'; sleep 0.1",
		Timeout: 300 * time.Millisecond,
		NotifyContext: func(context.Context, string) error {
			time.Sleep(600 * time.Millisecond)
			return nil
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || res.TimedOut {
		t.Fatalf("successful command was misclassified after observer cleanup: %+v", res)
	}

	timedOut, err := Run(testRequest(t, Request{
		Context: context.Background(),
		Command: "sleep 1",
		Timeout: 50 * time.Millisecond,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !timedOut.TimedOut || timedOut.ExitCode != -1 {
		t.Fatalf("genuine deadline overrun was not classified as timeout: %+v", timedOut)
	}
}

func TestSanitizedEnvironmentRemovesCommonProviderCredentials(t *testing.T) {
	entries := []string{
		"PATH=/bin",
		"GITHUB_PAT=pat",
		"SERVICE_AUTH=auth",
		"OPENAI_API_KEY=key",
		"NORMAL=value",
	}
	got := SanitizedEnvironmentFor(EnvironmentCommand, entries)
	joined := strings.Join(got, "\n")
	for _, secret := range []string{"GITHUB_PAT=", "SERVICE_AUTH=", "OPENAI_API_KEY="} {
		if strings.Contains(joined, secret) {
			t.Fatalf("sanitized environment retained %q: %v", secret, got)
		}
	}
	if !strings.Contains(joined, "PATH=/bin") || !strings.Contains(joined, "NORMAL=value") {
		t.Fatalf("sanitized environment removed non-secret values: %v", got)
	}
}

func TestLocalExecutionFailsClosedWithoutSandbox(t *testing.T) {
	if _, err := Run(Request{Context: context.Background(), Command: "true"}); err == nil {
		t.Fatal("Run started without a sandbox policy")
	}
	if _, err := RunArgv(context.Background(), []string{"true"}, Request{Context: context.Background()}); err == nil {
		t.Fatal("RunArgv started without a sandbox policy")
	}
	if _, err := StartArgv(StartRequest{Context: context.Background(), Argv: []string{"true"}}); err == nil {
		t.Fatal("StartArgv started without a sandbox policy")
	}
}

func TestSanitizedEnvironmentGitHubOptInIsNarrow(t *testing.T) {
	entries := []string{
		"PATH=/bin",
		"GH_TOKEN=gh-secret",
		"GITHUB_TOKEN=github-secret",
		"SERVICE_TOKEN=other-secret",
		"NORMAL=value",
	}
	got := SanitizedEnvironmentFor(EnvironmentGitHub, entries)
	joined := strings.Join(got, "\n")
	for _, want := range []string{"GH_TOKEN=gh-secret", "GITHUB_TOKEN=github-secret", "PATH=/bin", "NORMAL=value"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("GitHub environment omitted %q: %v", want, got)
		}
	}
	if strings.Contains(joined, "SERVICE_TOKEN=") {
		t.Fatalf("GitHub environment retained arbitrary token: %v", got)
	}
}

func TestExplicitEnvironmentCannotReintroduceHostProxyOrAgentSocket(t *testing.T) {
	policy := sandbox.New(t.TempDir())
	policy.SetScratchDir(t.TempDir())
	env, cleanup, err := prepareProcessEnvironment([]string{
		"PATH=/usr/bin:/bin",
		"HTTP_PROXY=http://127.0.0.1:8080",
		"CUSTOM_PROXY=socks5://127.0.0.1:1080",
		"SSH_AUTH_SOCK=/tmp/agent.sock",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/tmp/bus",
		"SERVER_TOKEN=explicit-mcp-token",
	}, policy)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	joined := strings.Join(env, "\n")
	for _, denied := range []string{"HTTP_PROXY=", "CUSTOM_PROXY=", "SSH_AUTH_SOCK=", "DBUS_SESSION_BUS_ADDRESS="} {
		if strings.Contains(joined, denied) {
			t.Fatalf("explicit child environment retained host delegation variable %q: %v", denied, env)
		}
	}
	if !strings.Contains(joined, "SERVER_TOKEN=explicit-mcp-token") {
		t.Fatalf("purpose-specific explicit credential was removed: %v", env)
	}
}

func TestProcessEnvironmentReusesSessionScratchForCaches(t *testing.T) {
	scratch := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(scratch); err == nil {
		scratch = resolved
	}
	policy := sandbox.New(t.TempDir())
	policy.SetScratchDir(scratch)

	first, cleanupFirst, err := prepareProcessEnvironment([]string{"PATH=/usr/bin:/bin"}, policy.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupFirst()
	second, cleanupSecond, err := prepareProcessEnvironment([]string{"PATH=/usr/bin:/bin"}, policy.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupSecond()
	for _, key := range []string{"HOME", "TMPDIR", "TMP", "TEMP", "XDG_CACHE_HOME", "GOCACHE", "npm_config_cache", "PIP_CACHE_DIR"} {
		firstValue, secondValue := environmentValue(first, key), environmentValue(second, key)
		if firstValue == "" || firstValue != secondValue {
			t.Fatalf("%s was not stable across session calls: first=%q second=%q", key, firstValue, secondValue)
		}
		if !withinPath(scratch, firstValue) {
			t.Fatalf("%s escaped the session scratch root: %q", key, firstValue)
		}
	}
}

func TestPreferNativeGitAvoidsXcrunShim(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS Command Line Tools adaptation")
	}
	if executableFile(nativeGitPath) == "" {
		t.Skip("Command Line Tools git is not installed")
	}
	env := []string{"PATH=/usr/bin:/bin"}
	argv, _ := preferNativeGit([]string{"git", "--version"}, "", env, t.TempDir())
	if argv[0] != nativeGitPath {
		t.Fatalf("argv git resolved to the xcrun shim: %v", argv)
	}
	_, shellEnv := preferNativeGit(nil, "git status", env, t.TempDir())
	if got := environmentValue(shellEnv, "PATH"); !strings.HasPrefix(got, filepath.Dir(nativeGitPath)+string(os.PathListSeparator)) {
		t.Fatalf("shell git PATH does not select the native tool: %q", got)
	}
}

func TestPreferNativeCLTPythonAvoidsXcrunShim(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS Command Line Tools adaptation")
	}
	nativePath := nativeCLTPythonPath()
	if nativePath == "" {
		t.Skip("Command Line Tools Python framework is not installed")
	}
	dir := t.TempDir()
	env := []string{"PATH=/usr/bin:/bin"}
	argv, _ := preferNativeCLTPython([]string{"/usr/bin/python3", "--version"}, "", env, dir, nativePath)
	if argv[0] != nativePath {
		t.Fatalf("argv python resolved to the xcrun shim: %v", argv)
	}
	_, shellEnv := preferNativeCLTPython(nil, "python3 --version", env, dir, nativePath)
	if got := environmentValue(shellEnv, "PATH"); !strings.HasPrefix(got, filepath.Dir(nativePath)+string(os.PathListSeparator)) {
		t.Fatalf("shell PATH does not select the native Python runtime: %q", got)
	}

	policy := sandbox.New(dir)
	addExecutionReadRoots(policy, argv, "", dir, env)
	wantRoot := pythonFrameworkVersionRoot(nativePath)
	if wantRoot == "" || executableReadRoot(nativePath, policy.Workspace) != wantRoot {
		t.Fatalf("Python executable read root = %q, want exact version root %q", executableReadRoot(nativePath, policy.Workspace), wantRoot)
	}
	found := false
	for _, root := range policy.ExecutionReadRoots {
		if root == wantRoot {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("execution policy did not discover Python runtime root %q: %v", wantRoot, policy.ExecutionReadRoots)
	}
}

func TestRunArgvAllowsGitReadTreeWithTemporaryIndex(t *testing.T) {
	dir := t.TempDir()
	if err := exec.Command("git", "init", "--quiet", dir).Run(); err != nil {
		t.Skipf("git init unavailable: %v", err)
	}
	index := filepath.Join(dir, "temporary-index")
	env := SanitizedEnvironment(os.Environ())
	env = append(env, "GIT_DIR="+filepath.Join(dir, ".git"), "GIT_INDEX_FILE="+index)
	result, err := RunArgv(context.Background(), []string{"git", "read-tree", "--empty"}, Request{
		Context: context.Background(),
		Dir:     dir,
		Env:     env,
		Sandbox: sandbox.New(dir),
	})
	if err != nil {
		t.Fatalf("git read-tree was incorrectly treated as interactive: %v (result=%+v)", err, result)
	}
	if result.ExitCode != 0 {
		t.Fatalf("git read-tree failed: result=%+v", result)
	}
	if _, err := os.Stat(index); err != nil {
		t.Fatalf("read-tree did not create the temporary index: %v", err)
	}
}

func TestSandboxLimitsApplyToAllExecutionEntrances(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS Seatbelt integration test")
	}
	runEntrances := func(label string, s *sandbox.Sandbox) error {
		check := func(name string, result Result, err error) error {
			if err != nil {
				return fmt.Errorf("%s/%s: %w", label, name, err)
			}
			if result.ExitCode != 0 || strings.TrimSpace(result.Stdout) != "64" {
				return fmt.Errorf("%s/%s: result=%+v, want stdout ulimit -n=64", label, name, result)
			}
			if strings.TrimSpace(result.Stderr) != "" {
				return fmt.Errorf("%s/%s: unexpected stderr %q", label, name, result.Stderr)
			}
			return nil
		}
		workspace := s.Workspace
		runResult, runErr := Run(Request{Context: context.Background(), Command: "ulimit -n", Dir: workspace, Sandbox: s})
		if err := check("Run", runResult, runErr); err != nil {
			return err
		}
		argvResult, argvErr := RunArgv(context.Background(), []string{"sh", "-c", "ulimit -n"}, Request{Context: context.Background(), Dir: workspace, Sandbox: s})
		if err := check("RunArgv", argvResult, argvErr); err != nil {
			return err
		}

		cmd, err := StartArgv(StartRequest{Context: context.Background(), Argv: []string{"sh", "-c", "ulimit -n"}, Dir: workspace, Sandbox: s})
		if err != nil {
			return fmt.Errorf("%s/StartArgv: %w", label, err)
		}
		defer CleanupStartedProcess(cmd)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("%s/StartArgv: %w", label, err)
		}
		if strings.TrimSpace(string(out)) != "64" {
			return fmt.Errorf("%s/StartArgv: stdout=%q, want 64", label, out)
		}
		if strings.TrimSpace(stderr.String()) != "" {
			return fmt.Errorf("%s/StartArgv: unexpected stderr %q", label, stderr.String())
		}
		return nil
	}

	s := sandbox.New(t.TempDir())
	s.Limits = &sandbox.Limits{MaxFiles: 64}
	if err := runEntrances("Seatbelt", s); err != nil {
		t.Fatal(err)
	}
}

func TestRunProgressChannelCannotBlockOrOutliveRun(t *testing.T) {
	// There is intentionally no progress consumer. A non-blocking channel send
	// must still drain the child and leave no observer goroutine behind.
	progress := make(chan string)
	res, err := Run(testRequest(t, Request{
		Context:     context.Background(),
		Command:     "head -c 2097152 /dev/zero",
		OutputLimit: 1024,
		Progress:    progress,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Fatalf("expected bounded progress result: %+v", res)
	}
}

func TestRunCancellableNotifyIsJoinedBeforeReturn(t *testing.T) {
	var once sync.Once
	observerStopped := make(chan struct{})
	res, err := Run(testRequest(t, Request{
		Context: context.Background(),
		Command: "printf 'progress\\n'; sleep 0.2",
		NotifyContext: func(ctx context.Context, _ string) error {
			// Model a permanently waiting observer that is nevertheless controlled
			// by the invocation context. Run must cancel and join it, rather than
			// returning after a fixed timeout with a leaked goroutine.
			<-ctx.Done()
			once.Do(func() { close(observerStopped) })
			return ctx.Err()
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "progress") {
		t.Fatalf("missing command output: %q", res.Output)
	}
	select {
	case <-observerStopped:
	default:
		t.Fatal("Run returned before joining the cancellable observer")
	}
}

func TestRunCancellationWithNotifyContinuesDraining(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	var once sync.Once
	runDone := make(chan struct{})
	var result Result
	var runErr error
	go func() {
		result, runErr = Run(testRequest(t, Request{
			Context:     ctx,
			Command:     "yes x",
			OutputLimit: 1024,
			NotifyContext: func(observerCtx context.Context, _ string) error {
				once.Do(func() { close(started) })
				<-observerCtx.Done()
				return observerCtx.Err()
			},
		}))
		close(runDone)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("NotifyContext did not observe streaming output")
	}
	cancel()
	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not finish after cancellation while draining output")
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
	if len(result.Output) > 1024 {
		t.Fatalf("output limit exceeded after cancellation: %d", len(result.Output))
	}
}

func TestRunCleansDescendantsAfterPipeWait(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	command := fmt.Sprintf("sleep 30 & echo $! > %s; printf done", shellQuote(pidFile))
	res, err := Run(testRequest(t, Request{Context: context.Background(), Command: command, Dir: dir}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || !strings.Contains(res.Output, "descendant cleanup") {
		t.Fatalf("expected descendant pipe cleanup marker, result=%+v", res)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(data))
	if pid == "" {
		t.Fatal("child pid was not recorded")
	}
	// The process group is killed before Run returns. Allow a short scheduling
	// window for macOS process reaping, but never accept a surviving sleep.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := exec.Command("kill", "-0", pid).Run(); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("descendant %s survived Run", pid)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func testRequest(t *testing.T, req Request) Request {
	t.Helper()
	if req.Dir == "" {
		req.Dir = t.TempDir()
	}
	if req.Sandbox == nil {
		req.Sandbox = sandbox.New(req.Dir)
	}
	return req
}
