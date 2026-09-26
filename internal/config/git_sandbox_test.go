package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxProtectsGitControlPaths(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is not installed")
	}
	workspace := t.TempDir()
	workspace = resolvedTestPath(t, workspace)
	gitConfigTestCommand(t, git, workspace, "init", "--quiet")
	customHooks := filepath.Join(workspace, "custom-hooks")
	if err := os.Mkdir(customHooks, 0o755); err != nil {
		t.Fatal(err)
	}
	gitConfigTestCommand(t, git, workspace, "config", "core.hooksPath", customHooks)

	cfg := Default()
	cfg.Workspace = workspace
	cfg.SessionDir = ""
	policy := cfg.Sandbox()
	protected := make(map[string]bool, len(policy.ProtectedDirs))
	for _, path := range policy.ProtectedDirs {
		protected[filepath.Clean(path)] = true
	}
	for _, want := range []string{
		filepath.Join(workspace, ".git", "config"),
		filepath.Join(workspace, ".git", "config.lock"),
		filepath.Join(workspace, ".git", "config.worktree"),
		filepath.Join(workspace, ".git", "config.worktree.lock"),
		filepath.Join(workspace, ".git", "hooks"),
		filepath.Join(workspace, ".git", "hooks.lock"),
		customHooks,
		customHooks + ".lock",
	} {
		if !protected[filepath.Clean(want)] {
			t.Errorf("Config.Sandbox omitted Git control path %s; protected paths: %v", want, policy.ProtectedDirs)
		}
	}
	gitConfigTestCommand(t, git, workspace, "config", "core.hooksPath", "/dev/null")
	policy = cfg.Sandbox()
	for _, path := range policy.ProtectedDirs {
		if filepath.Clean(path) == "/dev/null" {
			t.Fatal("unwritable /dev/null hooks target was unnecessarily protected")
		}
	}
}

func TestSandboxProtectsSymlinkedGitMarkerEntry(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is not installed")
	}
	base := resolvedTestPath(t, t.TempDir())
	repo := filepath.Join(base, "repo")
	workspace := filepath.Join(base, "linked-by-symlink")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitConfigTestCommand(t, git, repo, "init", "--quiet")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(workspace, ".git")
	if err := os.Symlink(filepath.Join(repo, ".git"), marker); err != nil {
		t.Fatal(err)
	}

	cfg := Default()
	cfg.Workspace = workspace
	cfg.SessionDir = ""
	policy := cfg.Sandbox()
	protectedMarker := false
	for _, path := range policy.ProtectedEntries {
		if path == marker {
			protectedMarker = true
			break
		}
	}
	if !protectedMarker {
		t.Fatalf("Config.Sandbox did not protect symlinked .git entry %s: %v", marker, policy.ProtectedEntries)
	}
	for _, path := range policy.ProtectedDirs {
		if filepath.Clean(path) == filepath.Clean(filepath.Join(repo, ".git")) {
			t.Fatal("symlink marker protection unexpectedly protected the whole Git directory")
		}
	}
}

func gitConfigTestCommand(t *testing.T, git, dir string, args ...string) {
	t.Helper()
	argv := append([]string{"-C", dir}, args...)
	cmd := exec.Command(git, argv...)
	cmd.Env = gitConfigTestEnvironment(os.Environ())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func gitConfigTestEnvironment(entries []string) []string {
	filtered := make([]string, 0, len(entries))
	for _, entry := range entries {
		key := entry
		if i := strings.IndexByte(entry, '='); i >= 0 {
			key = entry[:i]
		}
		if strings.HasPrefix(strings.ToUpper(key), "GIT_DIR") || strings.HasPrefix(strings.ToUpper(key), "GIT_WORK_TREE") || strings.HasPrefix(strings.ToUpper(key), "GIT_COMMON_DIR") {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func resolvedTestPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
