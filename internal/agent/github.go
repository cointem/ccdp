package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"unicode/utf8"

	"ccdp/internal/execution"
	"ccdp/internal/permissions"
)

// GitHub integration commands (Claude Code's /github, /pr-comments and
// /commit-push-pr). They shell out to the local git and gh CLIs in the
// workspace; the user drives them explicitly from the TUI.

// gitCmd runs git in the workspace and returns combined output + error.
func (a *Agent) gitCmd(args ...string) (string, error) {
	return a.localCommand(append([]string{"git"}, args...), false)
}

// ghCmd runs gh (GitHub CLI) in the workspace and returns combined output.
func (a *Agent) ghCmd(args ...string) (string, error) {
	return a.localCommand(append([]string{"gh"}, args...), true)
}

func (a *Agent) localCommand(argv []string, network bool) (string, error) {
	a.mu.Lock()
	ctx := a.rootCtx
	a.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	return a.localCommandContext(ctx, argv, network)
}

func (a *Agent) localCommandContext(ctx context.Context, argv []string, network bool) (string, error) {
	if len(argv) == 0 {
		return "", fmt.Errorf("empty command")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	workspace := a.cfg.Workspace
	sb := a.sandbox
	perms := a.perms
	a.mu.Unlock()
	if !externalCommandReadOnly(argv) {
		return "", fmt.Errorf("legacy GitHub helper only permits an explicitly read-only command")
	}
	command := strings.Join(argv, " ")
	if perms != nil {
		if denied, reason := perms.HardDeny("Bash", map[string]any{"command": command}); denied {
			return "", fmt.Errorf("command denied: %s", reason)
		}
		decision, reason := perms.Check("Bash", map[string]any{"command": command})
		if decision != permissions.DecisionAllow {
			return "", fmt.Errorf("command requires the typed runtime approval gate: %s", reason)
		}
	}
	if network && (sb == nil || !sb.NetworkAllowed()) {
		return "", fmt.Errorf("network command denied: no network capability is authorized")
	}
	actualArgv := argv
	readOnly := externalCommandReadOnly(argv)
	if readOnly && argv[0] == "git" {
		hardened, err := execution.ReadOnlyGitArgv(argv)
		if err != nil {
			return "", err
		}
		actualArgv = hardened
	}
	envPurpose := execution.EnvironmentGit
	if argv[0] == "gh" {
		// Only this fixed, read-only gh adapter receives GitHub credentials.
		// Ordinary git and shell commands continue to receive the generic
		// credential-free environment. The execution boundary owns the exact
		// GH_TOKEN/GITHUB_TOKEN allowlist.
		envPurpose = execution.EnvironmentGitHub
	}
	env := execution.SanitizedEnvironmentFor(envPurpose, os.Environ())
	if readOnly && actualArgv[0] == "git" {
		env = execution.ReadOnlyGitEnvironment(env)
	}
	res, err := execution.RunArgv(ctx, actualArgv, execution.Request{Context: ctx, Dir: workspace,
		Sandbox: sb, Env: env, OutputLimit: 512 * 1024})
	if err != nil {
		return strings.TrimSpace(res.Output), err
	}
	out := strings.TrimSpace(res.Output)
	if res.ExitCode != 0 {
		return out, fmt.Errorf("%s exited %d", argv[0], res.ExitCode)
	}
	return out, nil
}

// currentBranch returns the checked-out branch name, or "" when detached or
// when the directory is not a git repository.
func (a *Agent) currentBranch() string {
	branch, err := a.gitCmd("branch", "--show-current")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(branch)
}

// GitHubStatus reports the integration state: gh availability, origin remote,
// current branch and whether the branch already has a PR (/github).
func (a *Agent) GitHubStatus() string {
	return a.GitHubStatusContext(context.Background())
}

// GitHubStatusContext is the bounded-query variant used by the protocol
// report path. It keeps the same read-only argv/permission checks while
// allowing a caller to cancel a slow `gh` lookup.
func (a *Agent) GitHubStatusContext(ctx context.Context) string {
	if ctx == nil {
		ctx = context.Background()
	}
	var sb strings.Builder

	_, ghErr := exec.LookPath("gh")
	if ghErr != nil {
		sb.WriteString("gh: not installed — install the GitHub CLI to use /pr-comments and /commit-push-pr\n")
	} else {
		ver, err := a.localCommandContext(ctx, []string{"gh", "--version"}, true)
		if err != nil {
			fmt.Fprintf(&sb, "gh: unavailable — %s\n", commandDiagnostic(ver, err))
		} else {
			ver = firstLine(ver)
			if ver == "" {
				ver = "available"
			}
			fmt.Fprintf(&sb, "gh: %s\n", ver)
		}
	}

	remote, err := a.localCommandContext(ctx, []string{"git", "remote", "get-url", "origin"}, false)
	if err != nil {
		if isMissingRepository(remote) {
			remote = "(no origin remote)"
		} else {
			remote = "error: " + commandDiagnostic(remote, err)
		}
	}
	if remote == "" {
		remote = "(no origin remote)"
	}
	fmt.Fprintf(&sb, "remote: %s\n", sanitizeRemoteURL(remote))

	branchOutput, branchErr := a.localCommandContext(ctx, []string{"git", "branch", "--show-current"}, false)
	branch := strings.TrimSpace(branchOutput)
	if branchErr != nil {
		branch = ""
	}
	if branch == "" {
		fmt.Fprintf(&sb, "branch: (detached HEAD)\n")
	} else if ghErr != nil {
		fmt.Fprintf(&sb, "branch: %s\n", branch)
		sb.WriteString("pr: unavailable — gh not installed\n")
	} else {
		fmt.Fprintf(&sb, "branch: %s\n", branch)
		url, urlErr := a.localCommandContext(ctx, []string{"gh", "pr", "view", "--json", "url", "--jq", ".url", "--head", branch}, true)
		if urlErr != nil {
			fmt.Fprintf(&sb, "pr: unavailable — %s\n", commandDiagnostic(url, urlErr))
		} else if url != "" {
			fmt.Fprintf(&sb, "pr: %s\n", sanitizeRemoteURL(firstLine(url)))
		} else {
			sb.WriteString("pr: none yet for this branch — use /commit-push-pr\n")
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// PRComments fetches the current PR's description and review comments
// (Claude Code /pr-comments). Bounded to keep the TUI output readable.
func (a *Agent) PRComments() string {
	branch := a.currentBranch()
	if branch == "" {
		return "not on a branch — cannot find the current PR"
	}
	out, err := a.ghCmd("pr", "view", "--comments", "--head", branch)
	if err != nil {
		return fmt.Sprintf("GitHub CLI request failed for branch %s: %s", branch, commandDiagnostic(out, err))
	}
	return truncateUTF8(out, 8000)
}

func commandDiagnostic(output string, err error) string {
	if line := firstLine(output); line != "" {
		return line
	}
	if err != nil {
		if line := firstLine(err.Error()); line != "" {
			return line
		}
	}
	return "unknown error"
}

func isMissingRepository(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "not a git repository") ||
		strings.Contains(lower, "no such remote") ||
		strings.Contains(lower, "does not appear to be a git repository")
}

// sanitizeRemoteURL removes URL userinfo before a remote or PR URL reaches a
// report. It also handles scp-like remotes conservatively by only stripping
// userinfo from URLs that have an explicit scheme.
func sanitizeRemoteURL(remote string) string {
	remote = strings.TrimSpace(remote)
	// This helper is also used for report placeholders such as "(no origin
	// remote)" and command diagnostics. Treat non-URL text as text; url.Parse
	// would otherwise percent-escape it and make a useful diagnostic unreadable.
	if !strings.Contains(remote, "://") {
		return remote
	}
	if sanitized, err := sanitizeRequestEndpoint(remote); err == nil && sanitized != "" {
		return sanitized
	}
	// A URL-shaped value that cannot be parsed must not be echoed: malformed
	// credentials or query material are still credentials. Keep the report
	// useful without retaining any attacker-controlled bytes.
	return "[redacted-url]"
}

func truncateUTF8(value string, limit int) string {
	value = strings.ToValidUTF8(value, "�")
	if limit <= 0 || len(value) <= limit {
		return value
	}
	const suffix = "\n…[truncated]"
	budget := limit - len(suffix)
	if budget <= 0 {
		return validUTF8Prefix(suffix, limit)
	}
	return validUTF8Prefix(value, budget) + suffix
}

func validUTF8Prefix(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && cut < len(value) && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

// firstLine returns the first non-empty line of a string.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return s
}
