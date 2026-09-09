package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newTestRepo creates a temp git repo with one committed file.
func newTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	run("init", "-q", "-b", "main")
	run("config", "user.name", "t")
	run("config", "user.email", "t@t")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "-q", "-m", "initial")
	return dir
}

func gitCtx(dir string) *Context {
	return &Context{
		Context:    context.Background(),
		WorkingDir: dir,
		Args:       map[string]any{},
	}
}

func TestGitStatusTool(t *testing.T) {
	dir := newTestRepo(t)
	// Add an untracked file so status has output.
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := NewGitStatusTool().Run(gitCtx(dir))
	if err != nil {
		t.Fatalf("GitStatus: %v", err)
	}
	if !strings.Contains(out, "main") || !strings.Contains(out, "b.txt") {
		t.Errorf("GitStatus output missing branch/untracked:\n%s", out)
	}
}

func TestGitDiffTool(t *testing.T) {
	dir := newTestRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := NewGitDiffTool().Run(gitCtx(dir))
	if err != nil {
		t.Fatalf("GitDiff: %v", err)
	}
	if !strings.Contains(out, "+hello world") {
		t.Errorf("GitDiff output missing change:\n%s", out)
	}
}

func TestGitLogTool(t *testing.T) {
	dir := newTestRepo(t)
	out, err := NewGitLogTool().Run(gitCtx(dir))
	if err != nil {
		t.Fatalf("GitLog: %v", err)
	}
	if !strings.Contains(out, "initial") {
		t.Errorf("GitLog output missing commit message:\n%s", out)
	}
}

func TestGitCommitTool(t *testing.T) {
	dir := newTestRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "c.txt"), []byte("y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := gitCtx(dir)
	ctx.Args = map[string]any{"message": "add c.txt"}
	out, err := NewGitCommitTool().Run(ctx)
	if err != nil {
		t.Fatalf("GitCommit: %v", err)
	}
	if !strings.Contains(out, "1 file changed") && !strings.Contains(out, "c.txt") {
		t.Errorf("GitCommit output unexpected:\n%s", out)
	}
	// The file must now be committed.
	status, _ := NewGitStatusTool().Run(gitCtx(dir))
	if strings.Contains(status, "c.txt") {
		t.Errorf("c.txt should be committed:\n%s", status)
	}
}

func TestGitCommitRequiresMessage(t *testing.T) {
	dir := newTestRepo(t)
	_, err := NewGitCommitTool().Run(gitCtx(dir))
	if err == nil {
		t.Fatal("expected error for empty message")
	}
}

func TestHTMLToText(t *testing.T) {
	html := `<html><head><title>x</title><style>a{}</style></head><body>
	<p>Hello <b>world</b></p><ul><li>one</li><li>two</li></ul><script>bad()</script>
	</body></html>`
	got := htmlToText(html)
	if strings.Contains(got, "bad") || strings.Contains(got, "<") {
		t.Errorf("htmlToText leaked markup: %q", got)
	}
	for _, want := range []string{"Hello", "world", "one", "two"} {
		if !strings.Contains(got, want) {
			t.Errorf("htmlToText missing %q: %q", want, got)
		}
	}
}

func TestParseDuckDuckGo(t *testing.T) {
	html := `<div class="result">
	<a class="result__a" href="//example.com/a">Go Docs</a>
	<a class="result__snippet">The official Go documentation.</a>
	</div>
	<div class="result">
	<a class="result__a" href="//example.com/b">Go Blog</a>
	<a class="result__snippet">News from the Go team &amp; more.</a>
	</div>`
	results := parseDuckDuckGo(html, 10)
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d: %+v", len(results), results)
	}
	if results[0].Title != "Go Docs" || results[1].Snippet != "News from the Go team & more." {
		t.Errorf("unexpected parse: %+v", results)
	}
}
