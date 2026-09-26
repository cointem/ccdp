package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGitControlPathsCoverLinkedWorktreeMetadata(t *testing.T) {
	git := testGitBinary()
	if git == "" {
		t.Skip("Git is not installed")
	}
	base := t.TempDir()
	mainRepo := filepath.Join(base, "main repo")
	worktree := filepath.Join(base, "linked worktree")
	if err := os.MkdirAll(mainRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, git, mainRepo, "init", "--quiet")
	runGitTest(t, git, mainRepo, "config", "user.name", "Sandbox Test")
	runGitTest(t, git, mainRepo, "config", "user.email", "sandbox@example.invalid")
	runGitTest(t, git, mainRepo, "commit", "--allow-empty", "--quiet", "-m", "initial")
	runGitTest(t, git, mainRepo, "worktree", "add", "--quiet", "-b", "linked", worktree, "HEAD")
	customHooks := filepath.Join(mainRepo, "custom hooks")
	runGitTest(t, git, mainRepo, "config", "core.hooksPath", customHooks)

	if err := os.Mkdir(filepath.Join(worktree, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	paths := GitControlPaths(filepath.Join(worktree, "nested"))
	got := make(map[string]bool, len(paths))
	for _, path := range paths {
		got[filepath.Clean(path)] = true
	}
	gitDir := gitTestOutput(t, git, worktree, "rev-parse", "--path-format=absolute", "--absolute-git-dir")
	commonDir := gitTestOutput(t, git, worktree, "rev-parse", "--path-format=absolute", "--git-common-dir")
	wants := []string{
		filepath.Join(commonDir, "config"),
		filepath.Join(commonDir, "config.lock"),
		filepath.Join(gitDir, "config.worktree"),
		filepath.Join(gitDir, "config.worktree.lock"),
		filepath.Join(commonDir, "hooks"),
		filepath.Join(commonDir, "hooks.lock"),
		customHooks,
		customHooks + ".lock",
		filepath.Join(worktree, ".git"),
		filepath.Join(worktree, ".git.lock"),
		filepath.Join(gitDir, "gitdir"),
		filepath.Join(gitDir, "gitdir.lock"),
		filepath.Join(gitDir, "commondir"),
		filepath.Join(gitDir, "commondir.lock"),
	}
	for _, want := range wants {
		if !got[filepath.Clean(want)] {
			t.Errorf("Git control path missing: %s\nall paths: %v", want, paths)
		}
	}
	for _, writable := range []string{
		filepath.Join(gitDir, "index"),
		filepath.Join(commonDir, "objects"),
		filepath.Join(commonDir, "refs"),
	} {
		if got[filepath.Clean(writable)] {
			t.Errorf("normal Git data path was protected: %s", writable)
		}
	}
}

func TestGitControlPathsEnforceSeatbeltAndKeepCommitsWorking(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("real Seatbelt integration test")
	}
	if os.Getenv("CCDP_SEATBELT_INTEGRATION") != "1" {
		t.Skip("set CCDP_SEATBELT_INTEGRATION=1 to run nested Seatbelt integration")
	}
	if _, err := os.Stat(BackendPath); err != nil {
		t.Skip("sandbox-exec is unavailable")
	}
	git := testGitBinary()
	if git == "" {
		t.Skip("Git is not installed")
	}
	base := t.TempDir()
	mainRepo := filepath.Join(base, "main")
	worktree := filepath.Join(base, "worktree")
	if err := os.MkdirAll(mainRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, git, mainRepo, "init", "--quiet")
	runGitTest(t, git, mainRepo, "config", "user.name", "Sandbox Test")
	runGitTest(t, git, mainRepo, "config", "user.email", "sandbox@example.invalid")
	runGitTest(t, git, mainRepo, "commit", "--allow-empty", "--quiet", "-m", "initial")
	runGitTest(t, git, mainRepo, "worktree", "add", "--quiet", "-b", "sandbox-check", worktree, "HEAD")
	customHooks := filepath.Join(mainRepo, "custom-hooks")
	if err := os.MkdirAll(customHooks, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, git, mainRepo, "config", "core.hooksPath", customHooks)
	gitDir := gitTestOutput(t, git, worktree, "rev-parse", "--path-format=absolute", "--absolute-git-dir")
	commonDir := gitTestOutput(t, git, worktree, "rev-parse", "--path-format=absolute", "--git-common-dir")
	marker := filepath.Join(worktree, ".git")
	gitDirPointer := filepath.Join(gitDir, "gitdir")
	commonDirPointer := filepath.Join(gitDir, "commondir")

	policy := New(worktree)
	policy.AddDir(mainRepo)
	scratch := filepath.Join(base, "scratch")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	policy.AddScratchDir(scratch)
	for _, path := range GitControlPaths(worktree) {
		if policy.InWorkspace(path) {
			policy.AddProtectedDir(path)
		}
	}
	for _, path := range GitControlEntryPaths(worktree) {
		policy.AddProtectedEntry(path)
	}
	profile, err := policy.Profile()
	if err != nil {
		t.Fatal(err)
	}

	script := strings.Join([]string{
		"set -e",
		"if " + gitShellQuote(git) + " config user.name Attacker >/dev/null 2>&1; then echo config-write-allowed; exit 40; fi",
		"if printf forged > " + gitShellQuote(filepath.Join(commonDir, "config")) + " 2>/dev/null; then echo config-replacement-allowed; exit 41; fi",
		"if printf forged > " + gitShellQuote(filepath.Join(commonDir, "config.lock")) + " 2>/dev/null; then echo config-lock-write-allowed; exit 42; fi",
		"if printf forged > " + gitShellQuote(filepath.Join(gitDir, "config.worktree")) + " 2>/dev/null; then echo worktree-config-write-allowed; exit 43; fi",
		"if printf forged > " + gitShellQuote(customHooks) + "/pre-commit 2>/dev/null; then echo hook-write-allowed; exit 44; fi",
		"if printf forged > " + gitShellQuote(marker) + " 2>/dev/null; then echo worktree-marker-write-allowed; exit 45; fi",
		"if printf forged > " + gitShellQuote(gitDirPointer) + " 2>/dev/null; then echo gitdir-pointer-write-allowed; exit 46; fi",
		"if printf forged > " + gitShellQuote(commonDirPointer) + " 2>/dev/null; then echo commondir-pointer-write-allowed; exit 47; fi",
		"printf normal > tracked.txt",
		gitShellQuote(git) + " add tracked.txt",
		gitShellQuote(git) + " commit --quiet -m sandbox-commit",
		"test -z $(" + gitShellQuote(git) + " status --porcelain)",
		"echo git-control-protection-ok",
	}, "\n")
	cmd := exec.Command(BackendPath, "-p", profile, "--", "/bin/sh", "-c", script)
	cmd.Dir = worktree
	cmd.Env = []string{
		"PATH=/Library/Developer/CommandLineTools/usr/bin:/usr/bin:/bin",
		"HOME=" + scratch,
		"TMPDIR=" + scratch,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Seatbelt Git control test failed: %v\n%s\nprofile:\n%s", err, output, profile)
	}
	if !strings.Contains(string(output), "git-control-protection-ok") {
		t.Fatalf("Seatbelt Git control test did not finish: %s", output)
	}
	if got := gitTestOutput(t, git, mainRepo, "config", "user.name"); got != "Sandbox Test" {
		t.Fatalf("Git config changed despite protection: %q", got)
	}
}

func TestGitSymlinkMarkerSeatbeltBlocksUnlinkAndKeepsCommitsWorking(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("real Seatbelt integration test")
	}
	if os.Getenv("CCDP_SEATBELT_INTEGRATION") != "1" {
		t.Skip("set CCDP_SEATBELT_INTEGRATION=1 to run nested Seatbelt integration")
	}
	if _, err := os.Stat(BackendPath); err != nil {
		t.Skip("sandbox-exec is unavailable")
	}
	git := testGitBinary()
	if git == "" {
		t.Skip("Git is not installed")
	}
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	workspace := filepath.Join(base, "symlink-worktree")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, git, repo, "init", "--quiet")
	runGitTest(t, git, repo, "config", "user.name", "Sandbox Test")
	runGitTest(t, git, repo, "config", "user.email", "sandbox@example.invalid")
	runGitTest(t, git, repo, "commit", "--allow-empty", "--quiet", "-m", "initial")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(workspace, ".git")
	if err := os.Symlink(filepath.Join(repo, ".git"), marker); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, git, workspace, "status", "--short")

	policy := New(workspace)
	policy.AddDir(filepath.Join(repo, ".git"))
	scratch := filepath.Join(base, "scratch")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	policy.AddScratchDir(scratch)
	for _, path := range GitControlPaths(workspace) {
		if policy.InWorkspace(path) {
			policy.AddProtectedDir(path)
		}
	}
	for _, path := range GitControlEntryPaths(workspace) {
		policy.AddProtectedEntry(path)
	}
	profile, err := policy.Profile()
	if err != nil {
		t.Fatal(err)
	}
	script := strings.Join([]string{
		"set -e",
		"if /bin/rm .git 2>/dev/null; then echo git-marker-unlink-allowed; exit 50; fi",
		"test -L .git",
		"printf normal > tracked.txt",
		gitShellQuote(git) + " add tracked.txt",
		gitShellQuote(git) + " commit --quiet -m symlink-marker-commit",
		"test -L .git",
		"test -z $(" + gitShellQuote(git) + " status --porcelain)",
		"echo git-marker-protection-ok",
	}, "\n")
	cmd := exec.Command(BackendPath, "-p", profile, "--", "/bin/sh", "-c", script)
	cmd.Dir = workspace
	cmd.Env = []string{
		"PATH=/Library/Developer/CommandLineTools/usr/bin:/usr/bin:/bin",
		"HOME=" + scratch,
		"TMPDIR=" + scratch,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Seatbelt symlink-marker test failed: %v\n%s\nprofile:\n%s", err, output, profile)
	}
	if !strings.Contains(string(output), "git-marker-protection-ok") {
		t.Fatalf("Seatbelt symlink-marker test did not finish: %s", output)
	}
}

func runGitTest(t *testing.T, git, dir string, args ...string) {
	t.Helper()
	argv := append([]string{"-C", dir}, args...)
	cmd := exec.Command(git, argv...)
	cmd.Env = gitTestEnvironment(os.Environ())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func gitTestOutput(t *testing.T, git, dir string, args ...string) string {
	t.Helper()
	argv := append([]string{"-C", dir}, args...)
	cmd := exec.Command(git, argv...)
	cmd.Env = gitTestEnvironment(os.Environ())
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSuffix(string(output), "\n")
}

func gitShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func testGitBinary() string {
	if runtime.GOOS == "darwin" {
		for _, path := range []string{
			"/Library/Developer/CommandLineTools/usr/bin/git",
			"/Applications/Xcode.app/Contents/Developer/usr/bin/git",
			"/usr/bin/git",
		} {
			if info, err := os.Stat(path); err == nil && info.Mode()&0111 != 0 {
				return path
			}
		}
		return ""
	}
	path, _ := exec.LookPath("git")
	return path
}

func gitTestEnvironment(entries []string) []string {
	filtered := make([]string, 0, len(entries))
	for _, entry := range entries {
		key := entry
		if i := strings.IndexByte(entry, '='); i >= 0 {
			key = entry[:i]
		}
		switch strings.ToUpper(strings.TrimSpace(key)) {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_PREFIX":
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}
