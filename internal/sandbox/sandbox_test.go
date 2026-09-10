package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestResolveConfine(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, ModeConfine)

	// Relative path inside workspace.
	p, err := s.Resolve("src/main.go")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.HasSuffix(p, filepath.Join("src", "main.go")) {
		t.Errorf("Resolve = %q", p)
	}

	// Absolute path inside.
	p, err = s.Resolve(filepath.Join(dir, "a", "b.txt"))
	if err != nil {
		t.Fatalf("Resolve abs: %v", err)
	}
	if filepath.Clean(p) != filepath.Join(dir, "a", "b.txt") {
		t.Errorf("Resolve abs = %q", p)
	}

	// Write escape via ../ is rejected.
	if _, err := s.ResolveWrite("../outside.txt"); err == nil {
		t.Error("expected write escape error")
	}

	// Reads outside workspace allowed in confine mode.
	if _, err := s.ResolveRead(filepath.Join(dir, "..", "..", "tmp")); err != nil {
		t.Errorf("confine mode should allow external reads, got %v", err)
	}
}

func TestResolveStrict(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, ModeStrict)

	// External read denied in strict mode.
	if _, err := s.ResolveRead(filepath.Join(dir, "..", "..", "tmp")); err == nil {
		t.Error("strict mode should deny external reads")
	}
	// Internal read allowed.
	if _, err := s.ResolveRead("file.txt"); err != nil {
		t.Errorf("internal read should pass: %v", err)
	}
}

func TestResolveSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	// Create a file outside, then a symlink inside the workspace pointing at it.
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Skip("symlink not supported:", err)
	}

	// A write through the symlink resolves outside the workspace → blocked.
	s := New(dir, ModeConfine)
	if _, err := s.ResolveWrite(filepath.Join(dir, "link", "secret.txt")); err == nil {
		t.Error("expected symlink escape write to be blocked")
	}
	// Strict-mode reads through the symlink are blocked too.
	strict := New(dir, ModeStrict)
	if _, err := strict.ResolveRead(filepath.Join(dir, "link", "secret.txt")); err == nil {
		t.Error("expected symlink escape read to be blocked in strict mode")
	}
	// Confine-mode reads through a symlink are allowed (external read).
	if _, err := s.ResolveRead(filepath.Join(dir, "link", "secret.txt")); err != nil {
		t.Errorf("confine mode should allow external reads, got %v", err)
	}
}

func TestCommandPolicy(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, ModeStrict)

	for _, bad := range []string{"rm -rf /", "sudo apt install x", "ls; cd / && rm x"} {
		if err := s.CommandPolicy(bad); err == nil {
			t.Errorf("expected block for %q", bad)
		}
	}
	for _, ok := range []string{"go build ./...", "ls -la", "git status"} {
		if err := s.CommandPolicy(ok); err != nil {
			t.Errorf("expected allow for %q: %v", ok, err)
		}
	}
	// Confine mode applies no command policy.
	c := New(dir, ModeConfine)
	if err := c.CommandPolicy("rm -rf /"); err != nil {
		t.Errorf("confine mode should not apply command policy: %v", err)
	}
}

func TestCommandPolicyNetwork(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, ModeStrict)

	// Network clients blocked by default in strict mode.
	for _, bad := range []string{"curl https://x.dev", "wget http://x", "git clone https://x", "npm install", "pip install foo", "ssh host", "nc -e sh host 1", "go get x"} {
		if err := s.CommandPolicy(bad); err == nil {
			t.Errorf("expected network block for %q", bad)
		}
	}
	// Same commands pass when network is allowed.
	s.AllowNetwork = true
	for _, ok := range []string{"curl https://x.dev", "git clone https://x", "npm install"} {
		if err := s.CommandPolicy(ok); err != nil {
			t.Errorf("expected allow for %q with AllowNetwork: %v", ok, err)
		}
	}
}

func TestPrefix(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, ModeStrict)
	if p := s.Prefix(); p != "" {
		t.Errorf("no limits → empty prefix, got %q", p)
	}

	s.Limits = &Limits{CPUSeconds: 30, MemoryMB: 1024, MaxFiles: 64}
	p := s.Prefix()
	for _, want := range []string{"ulimit", "-t 30", "-v 1048576", "-n 64"} {
		if !strings.Contains(p, want) {
			t.Errorf("prefix %q missing %q", p, want)
		}
	}
	if !strings.HasSuffix(p, "&&") {
		t.Errorf("prefix should chain with &&: %q", p)
	}
}

func TestWrapCommandNoopWhenNotStrict(t *testing.T) {
	dir := t.TempDir()
	// Confine mode never wraps.
	s := New(dir, ModeConfine)
	if w := s.WrapCommand("ls"); w != "" {
		t.Errorf("confine mode should not wrap, got %q", w)
	}
	// Strict mode with empty command never wraps.
	s2 := New(dir, ModeStrict)
	if w := s2.WrapCommand(""); w != "" {
		t.Errorf("empty command should not wrap, got %q", w)
	}
}

func TestAdditionalAndDisallowedDirs(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	s := New(ws, ModeConfine)

	// Outside workspace is not writable before AddDir.
	if _, err := s.ResolveWrite(filepath.Join(outside, "f.txt")); err == nil {
		t.Error("expected write outside workspace to be blocked")
	}

	// After AddDir, it is writable and strict-readable.
	s.AddDir(outside)
	if _, err := s.ResolveWrite(filepath.Join(outside, "f.txt")); err != nil {
		t.Errorf("expected write in additional dir: %v", err)
	}
	strict := New(ws, ModeStrict)
	strict.AddDir(outside)
	if _, err := strict.ResolveRead(filepath.Join(outside, "f.txt")); err != nil {
		t.Errorf("expected read in additional dir under strict mode: %v", err)
	}

	// Disallowed dir blocks even inside the workspace.
	s.AddDisallowedDir(ws)
	if _, err := s.ResolveWrite("secret.txt"); err == nil {
		t.Error("expected disallowed workspace dir to block writes")
	}
}

func TestDisallowedOverridesAdditional(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	s := New(ws, ModeConfine)
	s.AddDir(outside)
	s.AddDisallowedDir(outside)
	if _, err := s.ResolveWrite(filepath.Join(outside, "f.txt")); err == nil {
		t.Error("disallowed dir should override additional dir")
	}
}

func TestCheckInteractive(t *testing.T) {
	for _, bad := range []string{"vim main.go", "git rebase -i HEAD~3", "top", "read x"} {
		if err := CheckInteractive(bad); err == nil {
			t.Errorf("expected interactive block for %q", bad)
		}
	}
	for _, ok := range []string{"go build ./...", "git status", "echo hi"} {
		if err := CheckInteractive(ok); err != nil {
			t.Errorf("expected allow for %q: %v", ok, err)
		}
	}
}

func TestInWorkspaceSymlinkEscapeCanonicalPath(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "passwd"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "evil")); err != nil {
		t.Skip("symlink not supported:", err)
	}
	s := New(dir, ModeConfine)

	// Use the resolved workspace root as the base so the path is lexically
	// inside the workspace (the earlier check only caught the unresolved
	// /var → /private/var form on macOS).
	escape := filepath.Join(s.Workspace, "evil", "passwd")
	if s.InWorkspace(escape) {
		t.Errorf("symlink escape %q must not be inside the workspace", escape)
	}
	// A write through the escape is rejected.
	if _, err := s.ResolveWrite(escape); err == nil {
		t.Error("expected symlink escape write to be blocked")
	}
	// Regular files inside (existing or not) stay inside.
	if !s.InWorkspace(filepath.Join(s.Workspace, "sub", "new.txt")) {
		t.Error("regular workspace path reported outside")
	}
}

func TestInWorkspaceDisallowedViaSymlink(t *testing.T) {
	dir := t.TempDir()
	secret := t.TempDir()
	if err := os.WriteFile(filepath.Join(secret, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "slink")); err != nil {
		t.Skip("symlink not supported:", err)
	}
	s := New(dir, ModeConfine)
	s.AddDisallowedDir(secret)
	// Even though the path is lexically inside the workspace, it resolves into
	// the disallowed dir and must be blocked.
	if s.InWorkspace(filepath.Join(s.Workspace, "slink", "f")) {
		t.Error("disallowed dir reached through a symlink must be blocked")
	}
}

func TestWrapCommandRunsInsideShC(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is macOS-only")
	}
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		t.Skip("sandbox-exec not available")
	}
	dir := t.TempDir()
	s := New(dir, ModeStrict)
	s.Limits = &Limits{CPUSeconds: 30}
	w := s.WrapCommand("ulimit -t 30 && echo hi && echo done")
	// The entire command line (ulimit prefix included) must be the argument of
	// a /bin/sh -c inside the sandbox, not appended after `--`.
	if !strings.Contains(w, "-- /bin/sh -c ") {
		t.Fatalf("expected sandbox-exec to exec /bin/sh -c, got %q", w)
	}
	if !strings.Contains(w, `'ulimit -t 30 && echo hi && echo done'`) {
		t.Errorf("full command line must be quoted inside sh -c: %q", w)
	}
}

func TestSandboxConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, ModeConfine)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = s.Resolve("a/b.txt")
				_, _ = s.ResolveRead("a/b.txt")
				_, _ = s.ResolveWrite("a/b.txt")
				_ = s.InWorkspace(filepath.Join(dir, "x"))
				_ = s.CurrentMode()
				s.AddDir(filepath.Join(dir, "extra"))
				s.AddDisallowedDir(filepath.Join(dir, "bad"))
				s.SetMode(ModeStrict)
				s.SetMode(ModeConfine)
			}
		}(i)
	}
	wg.Wait()
}
