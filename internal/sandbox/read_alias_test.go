package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveReadDeniedLeafThroughDirectoryAlias(t *testing.T) {
	root := t.TempDir()
	actual := filepath.Join(root, "actual")
	denied := filepath.Join(actual, "denied")
	for _, dir := range []string{actual, denied} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	allowed := filepath.Join(root, "allowed.md")
	if err := os.WriteFile(allowed, []byte("readable target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(allowed, filepath.Join(denied, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(actual, alias); err != nil {
		t.Fatal(err)
	}
	policy := New(root)
	policy.AddDisallowedDir(denied)
	if _, err := policy.ResolveRead(allowed); err != nil {
		t.Fatalf("target outside denied directory should remain readable: %v", err)
	}
	if _, err := policy.ResolveRead(filepath.Join(alias, "denied", "SKILL.md")); err == nil {
		t.Fatal("directory alias bypassed the denied original skill location")
	}
}
