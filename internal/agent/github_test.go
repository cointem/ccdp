package agent

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"ccdp/internal/config"
)

// newGithubAgent builds an agent whose workspace is the given dir.
func newGithubAgent(t *testing.T, dir string) *Agent {
	t.Helper()
	cfg := config.Default()
	cfg.Workspace = dir
	cfg.SessionDir = dir + "/sessions"
	events := make(chan Event, 64)
	ctrl := make(chan Control, 16)
	ag, err := New(&cfg, events, ctrl)
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

func TestCommitPushPRWithoutRemote(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "user.name", "ccdp test")
	git(t, dir, "checkout", "-q", "-b", "feature-y")
	if err := os.WriteFile(dir+"/x.txt", []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := newGithubAgent(t, dir)
	defer ag.Close()

	// No origin remote: commit succeeds, push fails gracefully.
	out := ag.CommitPushPR("add x.txt")
	if !strings.Contains(out, "staged") {
		t.Errorf("output missing staged step: %s", out)
	}
	if !strings.Contains(out, "commit") {
		t.Errorf("output missing commit step: %s", out)
	}
	if !strings.Contains(out, "push: failed") {
		t.Errorf("expected push to fail without a remote: %s", out)
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
