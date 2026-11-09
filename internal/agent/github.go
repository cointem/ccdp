package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"ccdp/internal/execution"
	"ccdp/internal/permissions"
)

func (a *Agent) localCommandContext(ctx context.Context, argv []string, network bool) (string, error) {
	if len(argv) == 0 {
		return "", fmt.Errorf("empty command")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	workspace := a.services.cfg.Workspace
	sb := a.services.sandbox
	perms := a.services.perms
	a.mu.Unlock()
	if !externalCommandReadOnly(argv) {
		return "", fmt.Errorf("GitHub status query only permits an explicitly read-only command")
	}
	command := strings.Join(argv, " ")
	if perms != nil {
		if denied, reason := perms.HardDeny("Bash", map[string]any{"command": command}); denied {
			return "", fmt.Errorf("command denied: %s", reason)
		}
		decision, reason := perms.Check("Bash", map[string]any{"command": command}, permissions.KindShell)
		if argv[0] == "git" {
			decision, reason = perms.CheckReadOnlyGit(argv)
		}
		if decision != permissions.DecisionAllow {
			return "", fmt.Errorf("command requires the typed runtime approval gate: %s", reason)
		}
	}
	if network && (sb == nil || !sb.NetworkAllowed()) {
		return "", fmt.Errorf("network command denied: no network capability is authorized")
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
	request := execution.Request{Context: ctx, Dir: workspace, Sandbox: sb, Env: env, OutputLimit: 512 * 1024}
	var res execution.Result
	var err error
	if argv[0] == "git" {
		res, err = execution.RunReadOnlyGit(ctx, argv, request)
	} else {
		res, err = execution.RunArgv(ctx, argv, request)
	}
	if err != nil {
		return strings.TrimSpace(res.Output), err
	}
	out := strings.TrimSpace(res.Output)
	if res.ExitCode != 0 {
		return out, fmt.Errorf("%s exited %d", argv[0], res.ExitCode)
	}
	return out, nil
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
		url, urlErr := a.localCommandContext(ctx, []string{"gh", "pr", "view", "--json", "url", "--jq", ".url", branch}, true)
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

// firstLine returns the first non-empty line of a string.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return s
}
