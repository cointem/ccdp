package fsops

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"ccdp/internal/atomicfile"
)

func TestPathLocksFollowCanonicalPathsAcrossReplacement(t *testing.T) {
	root, lockRoot := t.TempDir(), t.TempDir()
	t.Setenv("TMPDIR", lockRoot)
	workspace := filepath.Join(root, "project")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(workspace, alias); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workspace, "file")
	first, err := LockPath(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	for _, contents := range []string{"initial", "replacement"} {
		temp := filepath.Join(workspace, "candidate")
		if err := os.WriteFile(temp, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(temp, path); err != nil {
			t.Fatal(err)
		}
		next, err := LockPath(filepath.Join(alias, "file"))
		if next != nil {
			next.Close()
		}
		if !errors.Is(err, atomicfile.ErrLocked) {
			t.Fatalf("target replacement or alias bypassed the lock: %v", err)
		}
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(string(filepath.Separator), canonical)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(lockRoot, "ccdp-workspace-locks", "write", relative)); err != nil || !info.IsDir() {
		t.Fatalf("readable lock directory missing: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := LockPath(path)
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
}

func TestDirectoryLocksKeepPurposesAndChildPathsIndependent(t *testing.T) {
	root, lockRoot := t.TempDir(), t.TempDir()
	t.Setenv("TMPDIR", lockRoot)
	for _, purpose := range []string{"publish", "lease", "write", "delivery-plan"} {
		parent, err := lock(root, purpose)
		if err != nil {
			t.Fatal(err)
		}
		defer parent.Close()
		child, err := lock(filepath.Join(root, purpose+".lock"), purpose)
		if err != nil {
			t.Fatalf("child path collided with parent lock: %v", err)
		}
		defer child.Close()
		repeated, err := lock(root, purpose)
		if repeated != nil {
			repeated.Close()
		}
		if !errors.Is(err, atomicfile.ErrLocked) {
			t.Fatalf("second %s lock admitted: %v", purpose, err)
		}
	}
	if got, err := LockDelivery(root, "../outside"); err == nil {
		got.Close()
		t.Fatal("accepted delivery id containing a path separator")
	}
}
