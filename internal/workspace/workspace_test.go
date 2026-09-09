package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitignoreMatch(t *testing.T) {
	m, _ := NewMatcher()
	// Simulate a root .gitignore
	dir := t.TempDir()
	gitignore := "*.log\nnode_modules/\n!important.log\nbuild/**\n/top.go\nnested/deep.txt\n"
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(gitignore), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.LoadGitignores(dir); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		path   string
		isDir  bool
		ignore bool
	}{
		{"debug.log", false, true},
		{"node_modules", true, true},
		{"node_modules/pkg/index.js", false, true},
		{"important.log", false, false}, // negated
		{"build", true, true},
		{"build/out.js", false, true},
		{"top.go", false, true},      // anchored to root
		{"sub/top.go", false, false}, // anchored pattern doesn't match deeper
		{"nested/deep.txt", false, true},
		{"src/main.go", false, false},
	}
	for _, c := range cases {
		ignored, _ := m.Match(c.path, c.isDir)
		if ignored != c.ignore {
			t.Errorf("Match(%q, dir=%v) = %v, want %v", c.path, c.isDir, ignored, c.ignore)
		}
	}
}

func TestScanAndRepoMap(t *testing.T) {
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "src"), 0o755))
	must(os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "src", "util.go"), []byte("package src\n"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "README.md"), []byte("# hi\n"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "node_modules", "x.js"), []byte("x\n"), 0o644))
	must(os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.log\n"), 0o644))

	info, err := Scan(dir, 100)
	if err != nil {
		t.Fatal(err)
	}
	if info.FileCount != 3 {
		t.Fatalf("FileCount = %d, want 3 (node_modules ignored)", info.FileCount)
	}

	// Ensure the repo map lists all visible files (grouped by directory).
	rm := info.RepoMap(100)
	for _, want := range []string{"main.go", "util.go", "README.md", "src/"} {
		if !contains(rm, want) {
			t.Errorf("repo map missing %q\n%s", want, rm)
		}
	}
	if contains(rm, "node_modules") {
		t.Errorf("repo map should exclude node_modules\n%s", rm)
	}
}

func TestDetectGit(t *testing.T) {
	dir := t.TempDir()
	root, isRepo, branch := detectGit(dir)
	if isRepo {
		t.Fatalf("temp dir should not be a repo: %s", root)
	}
	gitdir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitdir, "HEAD"), []byte("ref: refs/heads/feature"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	root, isRepo, branch = detectGit(sub)
	if !isRepo || filepath.Clean(root) != filepath.Clean(dir) || branch != "feature" {
		t.Errorf("detectGit = (%s, %v, %q), want (%s, true, feature)", root, isRepo, branch, dir)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestLoadInstructionsIncludesUserLevel(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	os.Setenv("HOME", home)
	t.Cleanup(func() { os.Unsetenv("HOME") })

	// User-level instruction.
	os.MkdirAll(filepath.Join(home, ".ccdp"), 0o755)
	os.WriteFile(filepath.Join(home, ".ccdp", "AGENTS.md"), []byte("user rules\n"), 0o644)
	// Project-level instruction.
	os.WriteFile(filepath.Join(ws, "AGENTS.md"), []byte("project rules\n"), 0o644)

	out := LoadInstructions(ws)
	if out == "" {
		t.Fatal("expected instructions")
	}
	if !strings.Contains(out, "user rules") || !strings.Contains(out, "project rules") {
		t.Errorf("missing level: %q", out)
	}
}
