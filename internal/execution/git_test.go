package execution

import (
	"ccdp/internal/sandbox"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestReadOnlyGitDisablesAllPagersAndTextconv(t *testing.T) {
	for _, sub := range []string{"status", "branch", "diff", "log", "show", "blame", "grep"} {
		argv, err := ReadOnlyGitArgv([]string{"git", sub})
		if err != nil {
			t.Fatal(err)
		}
		if argv[1] != "--no-pager" {
			t.Fatalf("%s permits per-command pager: %q", sub, argv)
		}
		if !slices.Contains(argv, "core.fsmonitor=false") {
			t.Fatalf("%s permits fsmonitor", sub)
		}
		if sub == "diff" || sub == "log" || sub == "show" || sub == "blame" || sub == "grep" {
			if !slices.Contains(argv, "--no-textconv") {
				t.Fatalf("%s permits textconv", sub)
			}
		}
	}
}

func TestReadOnlyGitIgnoresRepositoryHelperCommands(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %q: %v %s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hello\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("*.txt diff=probe\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "fixture")
	marker := filepath.Join(dir, "helper-ran")
	helper := filepath.Join(dir, "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	run("config", "core.fsmonitor", helper)
	run("config", "diff.probe.textconv", helper)
	run("config", "diff.external", helper)
	run("config", "pager.branch", helper)
	for _, args := range [][]string{{"git", "status", "--short"}, {"git", "show", "HEAD:file.txt"}, {"git", "blame", "file.txt"}, {"git", "grep", "hello"}} {
		hardened, err := ReadOnlyGitArgv(args)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(hardened[0], hardened[1:]...)
		cmd.Dir = dir
		cmd.Env = ReadOnlyGitEnvironment(os.Environ())
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("query %q: %v %s", args, err, out)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("repository helper executed: %v", err)
	}
}

func TestReadOnlyGitNeverLazyFetchesMissingPromisorBlob(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %q: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "sample"), []byte("missing blob fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "sample")
	run("commit", "-qm", "fixture")
	blob := run("rev-parse", "HEAD:sample")
	if err := os.Remove(filepath.Join(dir, ".git", "objects", blob[:2], blob[2:])); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "ssh-helper-ran")
	helper := filepath.Join(dir, "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	run("config", "core.repositoryformatversion", "1")
	run("config", "extensions.partialClone", "origin")
	run("config", "remote.origin.promisor", "true")
	run("config", "remote.origin.url", "ssh://fixture.invalid/repo")
	run("config", "core.sshCommand", helper)
	// Establish that the missing object really activates the local stand-in
	// SSH helper; it exits immediately and never opens a network connection.
	probe := exec.Command("git", "show", "HEAD:sample")
	probe.Dir = dir
	probe.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=0", "GIT_ALLOW_PROTOCOL=ssh")
	_, _ = probe.CombinedOutput()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("fixture did not invoke SSH helper: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	argv, err := ReadOnlyGitArgv([]string{"git", "show", "HEAD:sample"})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = ReadOnlyGitEnvironment(append(os.Environ(), "GIT_NO_LAZY_FETCH=0", "GIT_ALLOW_PROTOCOL=ssh"))
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("missing blob query unexpectedly succeeded: %s", out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("read-only query invoked SSH helper: %v", err)
	}
}

func TestReadOnlyGitEnvironmentRejectsInheritedOverrides(t *testing.T) {
	environment := ReadOnlyGitEnvironment([]string{
		"PATH=/usr/bin:/bin", "LANG=C", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.sshCommand", "GIT_CONFIG_VALUE_0=helper",
		"GIT_CONFIG_PARAMETERS='core.pager=helper'", "GIT_EXEC_PATH=/host/helper", "GIT_DIR=/other/repo", "GIT_WORK_TREE=/other/tree",
		"GIT_INDEX_FILE=/other/index", "GIT_TRACE=/workspace/trace", "GIT_TRACE2_EVENT=/workspace/events", "GIT_SSH_COMMAND=helper",
		"GIT_ALLOW_PROTOCOL=ssh", "GIT_NO_LAZY_FETCH=0", "GIT_PAGER=helper", "PAGER=helper",
	})
	wantGit := map[string]string{"GIT_PAGER": "cat", "GIT_EXTERNAL_DIFF": "", "GIT_OPTIONAL_LOCKS": "0", "GIT_CONFIG_NOSYSTEM": "1", "GIT_NO_LAZY_FETCH": "1", "GIT_ALLOW_PROTOCOL": ""}
	seen := map[string]bool{}
	for _, entry := range environment {
		key, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") {
			want, ok := wantGit[key]
			if !ok || value != want || seen[key] {
				t.Errorf("unexpected Git environment %q", entry)
			}
			seen[key] = true
		}
	}
	for key := range wantGit {
		if !seen[key] {
			t.Errorf("missing %s", key)
		}
	}
	if !slices.Contains(environment, "PATH=/usr/bin:/bin") || !slices.Contains(environment, "LANG=C") || !slices.Contains(environment, "PAGER=cat") {
		t.Fatalf("lost safe environment: %q", environment)
	}
}

func TestReadOnlyGitPinsTrustedExecutableDespiteProjectPATH(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "git")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+":/usr/bin:/bin")
	argv, err := ReadOnlyGitArgv([]string{"git", "status"})
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(argv[0]) || argv[0] == script {
		t.Fatalf("untrusted git executable: %v", argv)
	}
}

func TestReadOnlyGitQueryBlocksCleanFilterAndInheritedTrace(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "input.txt")
	git("commit", "-qm", "fixture")
	if err := os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("*.txt filter=untrusted\n"), 0600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "helper-ran")
	git("config", "filter.untrusted.clean", "printf bad > '"+marker+"'; cat")
	git("config", "filter.untrusted.smudge", "cat")
	git("config", "filter.untrusted.required", "true")
	if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("after\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Establish that this repository's clean filter runs during the query
	// without the restricted execution boundary.
	hardened, err := ReadOnlyGitArgv([]string{"git", "diff"})
	if err != nil {
		t.Fatal(err)
	}
	probe := exec.Command(hardened[0], hardened[1:]...)
	probe.Dir = root
	probe.Env = ReadOnlyGitEnvironment(os.Environ())
	if output, err := probe.CombinedOutput(); err != nil {
		t.Fatalf("filter fixture: %v %s", err, output)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("filter fixture did not run: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(root, "inherited-trace")
	t.Setenv("GIT_TRACE", trace)
	policy := sandbox.New(root)
	policy.SetAllowNetwork(true)
	blocked, err := RunReadOnlyGit(context.Background(), []string{"git", "diff"}, Request{Dir: root, Sandbox: policy})
	if err != nil {
		t.Fatal(err)
	}
	if blocked.ExitCode == 0 {
		t.Fatalf("required untrusted filter was permitted: %+v", blocked)
	}
	for _, path := range []string{marker, trace} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("query wrote %s: %v", path, err)
		}
	}
	res, err := RunReadOnlyGit(context.Background(), []string{"git", "log", "--oneline"}, Request{Dir: root, Sandbox: policy})
	if err != nil || res.ExitCode != 0 || !strings.Contains(res.Stdout, "fixture") {
		t.Fatalf("ordinary query lost: %+v %v", res, err)
	}
}
