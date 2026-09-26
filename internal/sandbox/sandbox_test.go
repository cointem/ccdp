package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestResolveWorkspaceAndAuthorizedRoots(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)

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

	// Reads outside the workspace are allowed by default.
	if _, err := s.ResolveRead(filepath.Join(dir, "..", "..", "tmp")); err != nil {
		t.Errorf("external read should be allowed by default: %v", err)
	}
}

func TestResolveReadAllowsHostFilesWithoutGrantingWrite(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)

	outside := t.TempDir()
	if _, err := s.ResolveRead(outside); err != nil {
		t.Errorf("external read should be allowed by default: %v", err)
	}
	// A read-only root remains useful as a write carveout inside a writable root.
	if _, err := s.ResolveRead("file.txt"); err != nil {
		t.Errorf("workspace read should pass: %v", err)
	}
	s.AddReadOnlyDir(outside)
	if _, err := s.ResolveRead(outside); err != nil {
		t.Errorf("explicit read-only root should pass: %v", err)
	}
	if _, err := s.ResolveWrite(filepath.Join(outside, "new.txt")); err == nil {
		t.Error("read-only root must not grant writes")
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
	s := New(dir)
	if _, err := s.ResolveWrite(filepath.Join(dir, "link", "secret.txt")); err == nil {
		t.Error("expected symlink escape write to be blocked")
	}
	// Reads through a symlink escape follow the broad read policy.
	if _, err := s.ResolveRead(filepath.Join(dir, "link", "secret.txt")); err != nil {
		t.Errorf("expected symlink escape read to be allowed: %v", err)
	}
}

func TestPrefix(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
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

func TestAdditionalAndDisallowedDirs(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	s := New(ws)

	// Outside workspace is not writable before AddDir.
	if _, err := s.ResolveWrite(filepath.Join(outside, "f.txt")); err == nil {
		t.Error("expected write outside workspace to be blocked")
	}

	// After AddDir, it is writable and readable.
	s.AddDir(outside)
	if _, err := s.ResolveWrite(filepath.Join(outside, "f.txt")); err != nil {
		t.Errorf("expected write in additional dir: %v", err)
	}
	additional := New(ws)
	additional.AddDir(outside)
	if _, err := additional.ResolveRead(filepath.Join(outside, "f.txt")); err != nil {
		t.Errorf("expected read in additional dir: %v", err)
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
	s := New(ws)
	s.AddDir(outside)
	s.AddDisallowedDir(outside)
	if _, err := s.ResolveWrite(filepath.Join(outside, "f.txt")); err == nil {
		t.Error("disallowed dir should override additional dir")
	}
}

func TestReadOnlyOverridesWritableRoot(t *testing.T) {
	workspace := t.TempDir()
	readOnly := filepath.Join(workspace, "read-only")
	if err := os.Mkdir(readOnly, 0o700); err != nil {
		t.Fatal(err)
	}
	s := New(workspace)
	s.AddReadOnlyDir(readOnly)
	if _, err := s.ResolveRead(filepath.Join(readOnly, "data")); err != nil {
		t.Fatalf("read-only directory should remain readable: %v", err)
	}
	if _, err := s.ResolveWrite(filepath.Join(readOnly, "data")); err == nil {
		t.Fatal("read-only directory unexpectedly writable inside workspace")
	}
}

func TestExactAncestorsExposeMetadataOnlyAndRemainNarrow(t *testing.T) {
	got := exactAncestors([]string{"/private/var/folders/xy/T/workspace"})
	want := []string{"/private/var/folders/xy/T", "/private/var/folders/xy", "/private/var/folders", "/private/var", "/private", "/"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("exactAncestors = %v, want %v", got, want)
	}
}

func TestProtectedRuntimeDirCannotBeWritten(t *testing.T) {
	workspace := t.TempDir()
	protected := filepath.Join(workspace, ".ccdp")
	if err := os.MkdirAll(protected, 0o700); err != nil {
		t.Fatal(err)
	}
	s := New(workspace)
	s.AddDir(protected)
	s.AddProtectedDir(protected)
	if _, err := s.ResolveWrite(filepath.Join(protected, "settings.json")); err == nil {
		t.Fatal("write into protected runtime dir was accepted")
	}
}

func TestResolveWriteSymlinkToProtectedSubtree(t *testing.T) {
	workspace := t.TempDir()
	protected := filepath.Join(workspace, ".ccdp")
	if err := os.MkdirAll(protected, 0o700); err != nil {
		t.Fatal(err)
	}
	control := filepath.Join(protected, "settings.json")
	if err := os.WriteFile(control, []byte(`{"network_access":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(workspace, "runtime-link")
	if err := os.Symlink(protected, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	s := New(workspace)
	s.AddProtectedDir(protected)
	if _, err := s.ResolveWrite(filepath.Join(alias, "settings.json")); err == nil {
		t.Fatal("write through workspace symlink into protected subtree was accepted")
	}
}

func TestProtectedControlFileHardlinkCannotBeWritten(t *testing.T) {
	workspace := t.TempDir()
	protected := filepath.Join(workspace, ".ccdp")
	if err := os.MkdirAll(protected, 0o700); err != nil {
		t.Fatal(err)
	}
	control := filepath.Join(protected, "settings.json")
	if err := os.WriteFile(control, []byte(`{"mode":"default"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(workspace, "settings-alias.json")
	if err := os.Link(control, alias); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	s := New(workspace)
	s.AddProtectedDir(protected)
	if _, err := s.ResolveWrite(alias); err == nil {
		t.Fatal("protected control file hardlink was accepted for write")
	}
}

func TestDisallowedFileHardlinkCannotBeRead(t *testing.T) {
	workspace := t.TempDir()
	sensitive := t.TempDir()
	secret := filepath.Join(sensitive, "session.json")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(workspace, "session-alias.json")
	if err := os.Link(secret, alias); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	s := New(workspace)
	s.AddDisallowedDir(sensitive)
	if _, err := s.ResolveRead(alias); err == nil {
		t.Fatal("hardlink alias exposed a disallowed session file")
	}
	link := filepath.Join(workspace, "alias-symlink")
	if err := os.Symlink(alias, link); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveRead(link); err == nil {
		t.Fatal("symlink to hardlink alias exposed a disallowed session file")
	}
}

func TestCheckInteractive(t *testing.T) {
	for _, bad := range []string{"vim main.go", "git rebase -i HEAD~3", "top", "read x"} {
		if err := CheckInteractive(bad); err == nil {
			t.Errorf("expected interactive block for %q", bad)
		}
	}
	for _, ok := range []string{"go build ./...", "git status", "git read-tree HEAD", "echo hi"} {
		if err := CheckInteractive(ok); err != nil {
			t.Errorf("expected allow for %q: %v", ok, err)
		}
	}
}

func TestProfileShellSelectorExceptionIsExact(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin Seatbelt profile only")
	}
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		t.Skip("sandbox-exec not available")
	}
	s := New(t.TempDir())
	s.AddDisallowedDir("/private/var")
	profile, err := s.Profile()
	if err != nil {
		t.Fatal(err)
	}
	want := `(allow file-read* (require-all (literal "/private/var/select/sh")`
	if !strings.Contains(profile, want) {
		t.Fatalf("Seatbelt profile lacks exact shell selector read exception: %s", profile)
	}
	if !strings.Contains(profile, "(allow file-read*)\n") {
		t.Fatal("Seatbelt profile did not grant default host reads")
	}
	if !strings.Contains(profile, `(deny file-read* file-write* (subpath "/private/var"))`) {
		t.Fatal("explicitly denied directory was not excluded from broad reads")
	}
	for _, root := range [...]string{"/bin", "/usr/bin", "/sbin", "/usr/sbin"} {
		want := fmt.Sprintf(`(allow file-read-metadata file-test-existence (require-all (subpath "%s")`, root)
		if !strings.Contains(profile, want) {
			t.Fatalf("Seatbelt profile lacks narrow PATH lookup rule for %s: %s", root, profile)
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
	s := New(dir)

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
	s := New(dir)
	s.AddDisallowedDir(secret)
	// Even though the path is lexically inside the workspace, it resolves into
	// the disallowed dir and must be blocked.
	if s.InWorkspace(filepath.Join(s.Workspace, "slink", "f")) {
		t.Error("disallowed dir reached through a symlink must be blocked")
	}
}

func TestSandboxConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
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
				s.AddDir(filepath.Join(dir, "extra"))
				s.AddDisallowedDir(filepath.Join(dir, "bad"))
				s.AddReadOnlyDir(filepath.Join(dir, "readonly"))
				s.SetAllowNetwork(j%2 == 0)
			}
		}(i)
	}
	wg.Wait()
}

func TestExecutionWitnessAndPolicyTightening(t *testing.T) {
	workspace := t.TempDir()
	grant := t.TempDir()
	old := New(workspace)
	old.AddDir(grant)
	old.SetAllowNetwork(true)
	old.AddDisallowedDir(filepath.Join(workspace, "blocked"))
	if err := old.AddLocalService(LocalService{Direction: "connect", Protocol: "tcp", Port: 9000}); err != nil {
		t.Fatal(err)
	}

	widened := old.Snapshot()
	widened.AddDir(t.TempDir())
	if PolicyTightened(old, widened) {
		t.Fatal("adding an authorized directory was classified as tightening")
	}

	readOnly := old.Snapshot()
	readOnly.AddReadOnlyDir(filepath.Join(workspace, "read-only"))
	if !PolicyTightened(old, readOnly) {
		t.Fatal("adding a read-only carveout inside writable workspace was not classified as tightening")
	}

	protected := old.Snapshot()
	protected.AddProtectedDir(filepath.Join(workspace, "control"))
	if !PolicyTightened(old, protected) {
		t.Fatal("adding a protected control path was not classified as tightening")
	}

	entry := old.Snapshot()
	entry.AddProtectedEntry(filepath.Join(workspace, ".git"))
	if !PolicyTightened(old, entry) {
		t.Fatal("adding a protected directory entry was not classified as tightening")
	}

	narrowed := old.Snapshot()
	narrowed.AdditionalDirs = nil
	narrowed.SetAllowNetwork(false)
	if !PolicyTightened(old, narrowed) {
		t.Fatal("removing a directory/network grant was not classified as tightening")
	}

	derived := old.Snapshot()
	derived.MarkExternalExecution()
	if !old.ExternalExecutionPossible() {
		t.Fatal("execution witness did not survive a detached policy snapshot")
	}
	if !derived.ExternalExecutionPossible() {
		t.Fatal("execution witness is not sticky")
	}
}
