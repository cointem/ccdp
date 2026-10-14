package execution

import (
	"ccdp/internal/executable"
	"ccdp/internal/sandbox"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ReadOnlyGitArgv returns an argv for a git query which is insulated from
// repository configuration that can otherwise run a helper or a pager.  The
// caller must have already admitted the command as read-only; this helper only
// adds the execution hardening that applies to that admitted class.
//
// --no-pager overrides per-command pager.<name> settings as well as core.pager.
// Keep the options before the subcommand so git treats them as global options,
// and put diff options after the subcommand.  The latter covers `diff`, `log`
// and `show` even when their repository configuration requests text conversion
// or an external diff implementation.
func ReadOnlyGitArgv(argv []string) ([]string, error) {
	if len(argv) < 2 || argv[0] != "git" {
		return nil, fmt.Errorf("execution: read-only git argv must start with git and a subcommand")
	}

	path := executable.FindTrusted("git", os.Getenv("PATH"))
	if path == "/usr/bin/git" && executable.File(nativeGitPath) != "" {
		path = nativeGitPath
	}
	if path == "" {
		return nil, fmt.Errorf("execution: trusted git executable unavailable")
	}
	out := make([]string, 0, len(argv)+12)
	out = append(out, path, "--no-pager",
		"-c", "core.pager=cat",
		"-c", "pager.diff=false",
		"-c", "pager.log=false",
		"-c", "diff.external=",
		"-c", "core.fsmonitor=false",
		argv[1])
	if argv[1] == "diff" || argv[1] == "log" || argv[1] == "show" {
		out = append(out, "--no-ext-diff", "--no-textconv")
	} else if argv[1] == "blame" || argv[1] == "grep" {
		out = append(out, "--no-textconv")
	}
	out = append(out, argv[2:]...)
	return out, nil
}

// ReadOnlyGitEnvironment removes caller/host overrides for the Git helpers
// controlled at this boundary.  Values are replaced rather than appended to
// avoid duplicate environment keys with platform-dependent precedence.
func ReadOnlyGitEnvironment(entries []string) []string {
	// Remove the entire inherited Git namespace before credential filtering:
	// GIT_CONFIG_COUNT/KEY_n/VALUE_n form an indivisible configuration source,
	// and TRACE variables can write files even for otherwise read-only queries.
	// Explicit per-invocation overrides such as a temporary GIT_INDEX_FILE must
	// be appended by the owning typed adapter after this baseline is created.
	filtered := make([]string, 0, len(entries)+7)
	for _, entry := range entries {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(strings.ToUpper(key), "GIT_") || key == "PAGER" {
			continue
		}
		filtered = append(filtered, entry)
	}
	filtered = SanitizedEnvironmentFor(EnvironmentGit, filtered)
	return append(filtered,
		"GIT_PAGER=cat",
		"PAGER=cat",
		"GIT_EXTERNAL_DIFF=",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_CONFIG_NOSYSTEM=1",
		// A partial clone may otherwise fetch missing objects during show/diff,
		// invoking SSH or remote helpers despite query-only argv. The transport
		// deny also protects Git versions predating GIT_NO_LAZY_FETCH.
		"GIT_NO_LAZY_FETCH=1",
		"GIT_ALLOW_PROTOCOL=",
	)
}

// RunReadOnlyGit owns the entire query execution boundary: callers provide
// already-admitted Git argv, never a separately hardened command/environment.
func RunReadOnlyGit(ctx context.Context, argv []string, req Request) (Result, error) {
	prepared, err := ReadOnlyGitArgv(argv)
	if err != nil {
		return Result{}, err
	}
	req.readOnlyGit = true
	req.Env = queryGitEnvironment(req.Env)
	return RunArgv(ctx, prepared, req)
}

func StartReadOnlyGit(req StartRequest) (*Process, error) {
	prepared, err := ReadOnlyGitArgv(req.Argv)
	if err != nil {
		return nil, err
	}
	req.Argv, req.readOnlyGit = prepared, true
	req.Env = queryGitEnvironment(req.Env)
	return StartArgv(req)
}

func queryGitEnvironment(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	return ReadOnlyGitEnvironment(env)
}

func processProfile(policy *sandbox.Sandbox, argv []string, readOnlyGit bool) (string, error) {
	if readOnlyGit {
		return policy.ReadOnlyCommandProfile(argv[0])
	}
	return policy.Profile()
}

func verifyProcessProfile(ctx context.Context, profile string, argv []string, readOnlyGit bool) error {
	if !readOnlyGit {
		return verifyProfileContext(ctx, profile)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	// The narrow profile intentionally forbids the usual /bin/sh probe and
	// rlimit shell wrapper, because Git helpers use that same shell executable.
	probe := exec.CommandContext(probeCtx, sandbox.BackendPath, "-p", profile, "--", argv[0], "--version")
	if output, err := probe.CombinedOutput(); err != nil {
		return fmt.Errorf("execution: Seatbelt rejected read-only Git profile: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}
