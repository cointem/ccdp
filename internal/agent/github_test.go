package agent

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"unicode/utf8"

	"ccdp/internal/config"
	"ccdp/internal/execution"
)

// newGithubAgent builds an agent whose workspace is the given dir.
func newGithubAgent(t *testing.T, dir string) *Agent {
	t.Helper()
	cfg := config.Default()
	cfg.Workspace = dir
	cfg.SessionDir = dir + "/sessions"
	events := make(chan Event, 64)
	ag, err := New(&cfg, events)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ag
}

func TestGitHubStatusNoRepo(t *testing.T) {
	ag := newGithubAgent(t, t.TempDir())
	defer ag.Close()

	out := ag.GitHubStatus()
	if !strings.Contains(out, "no origin remote") {
		t.Errorf("status missing remote line: %s", out)
	}
	if !strings.Contains(out, "branch") {
		t.Errorf("status missing branch line: %s", out)
	}
}

func TestGitHubStatusHasRemote(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	git(t, dir, "remote", "add", "origin", "https://github.com/example/repo.git")
	git(t, dir, "checkout", "-q", "-b", "feature-x")

	ag := newGithubAgent(t, dir)
	defer ag.Close()

	out := ag.GitHubStatus()
	if !strings.Contains(out, "https://github.com/example/repo.git") {
		t.Errorf("status missing remote: %s", out)
	}
	if !strings.Contains(out, "feature-x") {
		t.Errorf("status missing branch: %s", out)
	}
}

func TestPRCommentsNoRepo(t *testing.T) {
	ag := newGithubAgent(t, t.TempDir())
	defer ag.Close()

	out := ag.PRComments()
	if !strings.Contains(out, "not on a branch") {
		t.Errorf("expected 'not on a branch': %s", out)
	}
}

func TestFirstLine(t *testing.T) {
	cases := map[string]string{
		"a\nb\nc":  "a",
		"   \na\n": "a",
		"single":   "single",
		"":         "",
	}
	for in, want := range cases {
		if got := firstLine(in); got != want {
			t.Errorf("firstLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGitHubEnvironmentOptsInOnlyGitHubTokens(t *testing.T) {
	entries := []string{
		"PATH=/usr/bin",
		"GH_TOKEN=gh-secret",
		"GITHUB_TOKEN=github-secret",
		"GITHUB_PAT=pat-secret",
		"OTHER_SECRET=other-secret",
	}
	got := execution.SanitizedEnvironmentFor(execution.EnvironmentGitHub, entries)
	joined := strings.Join(got, "\n")
	for _, want := range []string{"GH_TOKEN=gh-secret", "GITHUB_TOKEN=github-secret", "PATH=/usr/bin"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("GitHub environment omitted %q: %q", want, joined)
		}
	}
	for _, forbidden := range []string{"GITHUB_PAT=pat-secret", "OTHER_SECRET=other-secret"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("GitHub environment leaked %q: %q", forbidden, joined)
		}
	}
}

func TestGitHubReportRedactsRemoteCredentials(t *testing.T) {
	for _, tt := range []struct {
		input, want string
	}{
		{input: "https://user:secret@example.com/org/repo.git", want: "https://example.com/org/repo.git"},
		{input: "ssh://git:secret@example.com/org/repo.git", want: "ssh://example.com/org/repo.git"},
		{input: "https://example.com/org/repo.git?token=secret#frag", want: "https://example.com/org/repo.git"},
		{input: "https://example.com/path@name/repo.git", want: "https://example.com/path@name/repo.git"},
		{input: "git@example.com:org/repo.git", want: "git@example.com:org/repo.git"},
		{input: "https://user:secret@example.com/%zz", want: "[redacted-url]"},
	} {
		if got := sanitizeRemoteURL(tt.input); got != tt.want {
			t.Errorf("sanitizeRemoteURL(%q)=%q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestPRCommentTruncationPreservesUTF8(t *testing.T) {
	input := strings.Repeat("注", 5000)
	got := truncateUTF8(input, 8000)
	if !utf8.ValidString(got) {
		t.Fatalf("truncated PR comments are invalid UTF-8")
	}
	if !strings.HasSuffix(got, "\n…[truncated]") {
		t.Fatalf("truncated PR comments lack marker: %q", got[len(got)-min(len(got), 40):])
	}
}

func TestGitHubStatusSurfacesGHFailure(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	git(t, dir, "checkout", "-q", "-b", "feature-x")
	bin := t.TempDir()
	gh := bin + "/gh"
	if err := os.WriteFile(gh, []byte("#!/bin/sh\necho forbidden >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	ag := newGithubAgent(t, dir)
	defer ag.Close()
	ag.perms.SetAllowAll(true)
	ag.sandbox.SetAllowNetwork(true)

	out := ag.GitHubStatus()
	if !strings.Contains(out, "gh: unavailable") || !strings.Contains(out, "forbidden") {
		t.Fatalf("GitHub status hid gh failure: %s", out)
	}
	if !strings.Contains(out, "pr: unavailable") || strings.Contains(out, "pr: none yet") {
		t.Fatalf("GitHub status misreported PR failure: %s", out)
	}
}

// git runs a git command in dir for test setup.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
