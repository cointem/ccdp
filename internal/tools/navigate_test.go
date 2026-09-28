package tools

import (
	"ccdp/internal/sandbox"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotReadBoundaryResolvesAliasesAndLinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if e := os.WriteFile(filepath.Join(root, "inside"), []byte("ok"), 0644); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(outside, "secret"), []byte("private"), 0644); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "link")); e != nil {
		t.Fatal(e)
	}
	c := &Context{Context: context.Background(), WorkingDir: root, ReadRoot: root, Sandbox: sandbox.New(root)}
	if _, e := c.ResolveRead("inside"); e != nil {
		t.Fatalf("own snapshot denied: %v", e)
	}
	if _, e := c.ResolveRead("link"); e == nil {
		t.Fatal("followed snapshot link outside root")
	}
}
