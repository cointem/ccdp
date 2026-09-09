package tools

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// gitMaxOutput caps a single git tool's returned text.
const gitMaxOutput = 32000

// runGit executes git in the workspace directory with a timeout and returns
// the combined output and exit code.
func runGit(ctx *Context, args ...string) (string, int, error) {
	timeout := ctx.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "git", args...)
	cmd.Dir = ctx.WorkingDir

	out := &strings.Builder{}
	cmd.Stdout = out
	cmd.Stderr = out

	err := cmd.Run()
	code := 0
	if cctx.Err() == context.DeadlineExceeded {
		return out.String() + "\n[git command timed out]", -1, nil
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			return out.String(), code, fmt.Errorf("git: %w", err)
		}
	}
	return out.String(), code, nil
}

// truncateGit caps output at gitMaxOutput with an explicit marker.
func truncateGit(s string) string {
	if len(s) <= gitMaxOutput {
		return s
	}
	return s[:gitMaxOutput] + fmt.Sprintf("\n…[git output truncated at %d chars]", gitMaxOutput)
}

// ---------- GitStatus ----------

// GitStatusTool reports the working tree status.
type GitStatusTool struct{}

// NewGitStatusTool creates the git status tool.
func NewGitStatusTool() *GitStatusTool { return &GitStatusTool{} }

func (t *GitStatusTool) Name() string { return "GitStatus" }

func (t *GitStatusTool) Description() string {
	return `Show the current git status: branch, staged/unstaged/untracked files.
Use this before editing or committing to understand the repository state.
Read-only and always allowed.`
}

func (t *GitStatusTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"short": map[string]any{
				"type":        "boolean",
				"description": "Use porcelain (machine-readable) output instead of the human-readable format. Default false.",
			},
		},
	}
}

func (t *GitStatusTool) Run(ctx *Context) (string, error) {
	args := []string{"status"}
	if BoolArg(ctx.Args, "short", false) {
		args = []string{"status", "--short", "--branch"}
	}
	out, code, err := runGit(ctx, args...)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("git %s (exit %d)\n%s", strings.Join(args, " "), code, truncateGit(out)), nil
}

// ---------- GitDiff ----------

// GitDiffTool shows working tree changes.
type GitDiffTool struct{}

// NewGitDiffTool creates the git diff tool.
func NewGitDiffTool() *GitDiffTool { return &GitDiffTool{} }

func (t *GitDiffTool) Name() string { return "GitDiff" }

func (t *GitDiffTool) Description() string {
	return `Show uncommitted changes as a diff. Options:
- staged=true: show only staged (index) changes (git diff --cached)
- base="HEAD" or "<commit>": diff against a commit instead of the working tree
- stat=true: show only a change summary (files + +/- counts)
Read-only and always allowed.`
}

func (t *GitDiffTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"staged": map[string]any{
				"type":        "boolean",
				"description": "Diff the staged (index) changes only. Default false.",
			},
			"base": map[string]any{
				"type":        "string",
				"description": "Commit to diff against (e.g. HEAD, HEAD~1, <sha>). Default: working tree vs index.",
			},
			"stat": map[string]any{
				"type":        "boolean",
				"description": "Show only the diffstat summary. Default false.",
			},
		},
	}
}

func (t *GitDiffTool) Run(ctx *Context) (string, error) {
	args := []string{"diff"}
	if BoolArg(ctx.Args, "staged", false) {
		args = append(args, "--cached")
	}
	if base := StringArg(ctx.Args, "base", ""); base != "" {
		args = append(args, base)
	}
	if BoolArg(ctx.Args, "stat", false) {
		args = append(args, "--stat")
	}
	out, code, err := runGit(ctx, args...)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Sprintf("git %s (exit %d): no changes", strings.Join(args, " "), code), nil
	}
	return fmt.Sprintf("git %s (exit %d)\n%s", strings.Join(args, " "), code, truncateGit(out)), nil
}

// ---------- GitLog ----------

// GitLogTool shows commit history.
type GitLogTool struct{}

// NewGitLogTool creates the git log tool.
func NewGitLogTool() *GitLogTool { return &GitLogTool{} }

func (t *GitLogTool) Name() string { return "GitLog" }

func (t *GitLogTool) Description() string {
	return `Show recent commit history as one line per commit (hash, author, date,
subject). Optionally restrict to a path or author. Read-only and always allowed.`
}

func (t *GitLogTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"count": map[string]any{
				"type":        "integer",
				"description": "Number of commits to show. Default 20, max 100.",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "Only commits touching this file/directory.",
			},
			"author": map[string]any{
				"type":        "string",
				"description": "Only commits by this author (substring match).",
			},
		},
	}
}

func (t *GitLogTool) Run(ctx *Context) (string, error) {
	n := IntArg(ctx.Args, "count", 20)
	if n < 1 {
		n = 1
	}
	if n > 100 {
		n = 100
	}
	args := []string{"log", "--oneline", "-n", fmt.Sprintf("%d", n)}
	if author := StringArg(ctx.Args, "author", ""); author != "" {
		args = append(args, "--author="+author)
	}
	if p := StringArg(ctx.Args, "path", ""); p != "" {
		args = append(args, "--", p)
	}
	out, code, err := runGit(ctx, args...)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Sprintf("git %s (exit %d): no commits", strings.Join(args, " "), code), nil
	}
	return fmt.Sprintf("git %s (exit %d)\n%s", strings.Join(args, " "), code, truncateGit(out)), nil
}

// ---------- GitCommit ----------

// GitCommitTool creates a commit.
type GitCommitTool struct{}

// NewGitCommitTool creates the git commit tool.
func NewGitCommitTool() *GitCommitTool { return &GitCommitTool{} }

func (t *GitCommitTool) Name() string { return "GitCommit" }

func (t *GitCommitTool) Description() string {
	return `Create a git commit with the given message. By default all changes
(including untracked files) are staged first, so the commit is complete; set
staged_only=true to commit only what is already staged. This mutates history
and requires approval in default/acceptEdits modes. Never run with --no-verify.`
}

func (t *GitCommitTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"message": map[string]any{
				"type":        "string",
				"description": "The commit message. Use a concise imperative summary.",
			},
			"staged_only": map[string]any{
				"type":        "boolean",
				"description": "Commit only already-staged changes. Default false (stages everything first).",
			},
		},
		"required": []string{"message"},
	}
}

func (t *GitCommitTool) Run(ctx *Context) (string, error) {
	msg := StringArg(ctx.Args, "message", "")
	if strings.TrimSpace(msg) == "" {
		return "", fmt.Errorf("GitCommit: message is required")
	}

	var sb strings.Builder
	if !BoolArg(ctx.Args, "staged_only", false) {
		out, code, err := runGit(ctx, "add", "-A")
		sb.WriteString(fmt.Sprintf("git add -A (exit %d)\n%s\n", code, truncateGit(out)))
		if err != nil {
			return sb.String(), err
		}
	}

	out, code, err := runGit(ctx, "commit", "-m", msg)
	sb.WriteString(fmt.Sprintf("git commit -m %q (exit %d)\n%s", msg, code, truncateGit(out)))
	if err != nil {
		return sb.String(), err
	}
	return sb.String(), nil
}
