package runtimepath

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateRootRejectsNonDirectoriesAndReportsFullPath(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := EnsurePrivate(filepath.Join(file, "child"))
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("missing path error: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(file)
	if err != nil {
		t.Fatal(err)
	}
	if pathErr.Path != canonical {
		t.Fatalf("error path=%q, want %q", pathErr.Path, canonical)
	}
	for _, path := range []string{"", "/"} {
		if _, err := EnsurePrivate(path); err == nil {
			t.Fatalf("accepted invalid private root %q", path)
		}
	}
}

func TestPrivateRootRejectsLinksAndInsecureMode(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtime")
	path, err := EnsurePrivate(root)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal(info, err)
	}
	alias := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsurePrivate(filepath.Join(alias, "child")); err == nil {
		t.Fatal("followed a runtime symlink")
	}
	if err := os.Chmod(path, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsurePrivate(path); err == nil {
		t.Fatal("accepted insecure private root")
	}
}
