package execution

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	res, err := Run(Request{
		Context:     ctx,
		Command:     "head -c 2097152 /dev/zero",
		OutputLimit: 64 * 1024,
		Progress:    make(chan string),
	})
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
	res, err := Run(Request{
		Context:     context.Background(),
		Command:     "for i in $(seq 1 100); do echo line; done",
		OutputLimit: 1024,
		NotifyContext: func(context.Context, string) error {
			time.Sleep(500 * time.Millisecond)
			return nil
		},
	})
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
	res, err := Run(Request{Context: ctx, Command: "sleep 10"})
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
	res, err := Run(Request{
		Context: context.Background(),
		Command: "printf 'done\\n'; sleep 0.1",
		Timeout: 300 * time.Millisecond,
		NotifyContext: func(context.Context, string) error {
			time.Sleep(600 * time.Millisecond)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || res.TimedOut {
		t.Fatalf("successful command was misclassified after observer cleanup: %+v", res)
	}

	timedOut, err := Run(Request{
		Context: context.Background(),
		Command: "sleep 1",
		Timeout: 50 * time.Millisecond,
	})
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
	})
	if err != nil {
		t.Fatalf("git read-tree was incorrectly treated as interactive: %v (result=%+v)", err, result)
	}
	if _, err := os.Stat(index); err != nil {
		t.Fatalf("read-tree did not create the temporary index: %v", err)
	}
}

func TestSandboxLimitsApplyToAllExecutionEntrances(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
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
		runResult, runErr := Run(Request{Context: context.Background(), Command: "ulimit -n", Dir: workspace, Shell: "/bin/zsh", Sandbox: s})
		if err := check("Run", runResult, runErr); err != nil {
			return err
		}
		argvResult, argvErr := RunArgv(context.Background(), []string{"sh", "-c", "ulimit -n"}, Request{Context: context.Background(), Dir: workspace, Shell: "/bin/zsh", Sandbox: s})
		if err := check("RunArgv", argvResult, argvErr); err != nil {
			return err
		}

		cmd, err := StartArgv(StartRequest{Context: context.Background(), Argv: []string{"sh", "-c", "ulimit -n"}, Dir: workspace, Sandbox: s})
		if err != nil {
			return fmt.Errorf("%s/StartArgv: %w", label, err)
		}
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

	s := sandbox.New(t.TempDir(), sandbox.ModeConfine)
	s.Limits = &sandbox.Limits{MaxFiles: 64}
	if err := runEntrances("confine", s); err != nil {
		t.Fatal(err)
	}

	// Strict mode uses the real Darwin backend when available. Nested CI/Codex
	// sandboxes may intentionally deny sandbox-exec, so retain the confine
	// assertions above and record that environmental skip explicitly.
	strict := sandbox.New(t.TempDir(), sandbox.ModeStrict)
	strict.Limits = &sandbox.Limits{MaxFiles: 64}
	if err := strict.StrictBackendError(); err == nil {
		if err := runEntrances("strict", strict); err != nil {
			if strings.Contains(err.Error(), "sandbox_apply: Operation not permitted") {
				t.Logf("strict limit entrance unavailable in this host sandbox: %v", err)
			} else {
				t.Fatal(err)
			}
		}
	} else {
		t.Logf("strict limit entrance skipped: %v", err)
	}
}

func TestRunProgressChannelCannotBlockOrOutliveRun(t *testing.T) {
	// There is intentionally no progress consumer. A non-blocking channel send
	// must still drain the child and leave no observer goroutine behind.
	progress := make(chan string)
	res, err := Run(Request{
		Context:     context.Background(),
		Command:     "head -c 2097152 /dev/zero",
		OutputLimit: 1024,
		Progress:    progress,
	})
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
	res, err := Run(Request{
		Context: context.Background(),
		Command: "printf 'progress\\n'; sleep 0.2",
		Shell:   "/bin/sh",
		NotifyContext: func(ctx context.Context, _ string) error {
			// Model a permanently waiting observer that is nevertheless controlled
			// by the invocation context. Run must cancel and join it, rather than
			// returning after a fixed timeout with a leaked goroutine.
			<-ctx.Done()
			once.Do(func() { close(observerStopped) })
			return ctx.Err()
		},
	})
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
		result, runErr = Run(Request{
			Context:     ctx,
			Command:     "yes x",
			Shell:       "/bin/sh",
			OutputLimit: 1024,
			NotifyContext: func(observerCtx context.Context, _ string) error {
				once.Do(func() { close(started) })
				<-observerCtx.Done()
				return observerCtx.Err()
			},
		})
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
	res, err := Run(Request{Context: context.Background(), Command: command, Shell: "/bin/sh"})
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
