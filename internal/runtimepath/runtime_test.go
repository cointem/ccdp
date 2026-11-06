package runtimepath

import (
	"os"
	"path/filepath"
	"testing"
)

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
