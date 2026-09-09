package agent

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// GitHub integration commands (Claude Code's /github, /pr-comments and
// /commit-push-pr). They shell out to the local git and gh CLIs in the
// workspace; the user drives them explicitly from the TUI.

// gitCmd runs git in the workspace and returns combined output + error.
func (a *Agent) gitCmd(args ...string) (string, error) {
	var out bytes.Buffer
	cmd := exec.Command("git", args...)
	cmd.Dir = a.cfg.Workspace
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

// ghCmd runs gh (GitHub CLI) in the workspace and returns combined output.
func (a *Agent) ghCmd(args ...string) (string, error) {
	var out bytes.Buffer
	cmd := exec.Command("gh", args...)
	cmd.Dir = a.cfg.Workspace
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
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
	var sb strings.Builder

	if _, err := exec.LookPath("gh"); err != nil {
		sb.WriteString("gh: not installed — install the GitHub CLI to use /pr-comments and /commit-push-pr\n")
	} else {
		ver, _ := a.ghCmd("--version")
		if first := strings.SplitN(ver, "\n", 2); len(first) > 0 && first[0] != "" {
			ver = first[0]
		}
		fmt.Fprintf(&sb, "gh: %s\n", ver)
	}

	remote, err := a.gitCmd("remote", "get-url", "origin")
	if err != nil {
		remote = ""
	}
	if remote == "" {
		remote = "(no origin remote)"
	}
	fmt.Fprintf(&sb, "remote: %s\n", remote)

	branch := a.currentBranch()
	if branch == "" {
		fmt.Fprintf(&sb, "branch: (detached HEAD)\n")
	} else {
		fmt.Fprintf(&sb, "branch: %s\n", branch)
		url, _ := a.ghCmd("pr", "view", "--json", "url", "--jq", ".url", "--head", branch)
		if url != "" {
			fmt.Fprintf(&sb, "pr: %s\n", url)
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
		return fmt.Sprintf("no PR found for branch %s: %s", branch, out)
	}
	if len(out) > 8000 {
		out = out[:8000] + "\n…[truncated]"
	}
	return out
}

// CommitPushPR commits all changes, pushes the current branch and opens a PR
// (Claude Code /commit-push-pr). Each step reports its own outcome so partial
// failures are visible.
func (a *Agent) CommitPushPR(message string) string {
	if strings.TrimSpace(message) == "" {
		message = "work in progress"
	}
	var steps []string

	if out, err := a.gitCmd("add", "-A"); err != nil {
		return "git add failed: " + out
	}
	steps = append(steps, "staged: all changes")

	if out, err := a.gitCmd("commit", "-m", message); err != nil {
		steps = append(steps, "commit: "+firstLine(out)+" (tree may be clean)")
	} else {
		steps = append(steps, "commit: "+firstLine(out))
	}

	branch := a.currentBranch()
	if branch == "" {
		steps = append(steps, "push: skipped — not on a branch")
		return strings.Join(steps, "\n")
	}
	if out, err := a.gitCmd("push", "-u", "origin", "HEAD"); err != nil {
		steps = append(steps, "push: failed — "+firstLine(out))
		return strings.Join(steps, "\n")
	}
	steps = append(steps, "push: pushed "+branch+" to origin")

	if _, err := exec.LookPath("gh"); err != nil {
		steps = append(steps, "pr: gh not installed — open the push link to create the PR")
		return strings.Join(steps, "\n")
	}
	if out, err := a.ghCmd("pr", "create", "--fill", "--head", branch); err != nil {
		steps = append(steps, "pr: creation failed — "+firstLine(out))
	} else {
		steps = append(steps, "pr: "+firstLine(out))
	}
	return strings.Join(steps, "\n")
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
