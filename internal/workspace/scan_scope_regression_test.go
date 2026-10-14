package workspace

import (
	"ccdp/internal/sandbox"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestWorkspaceScanSubdirectoryIgnoreScope(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{".git", "sub"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{
		".git/HEAD":         "ref: refs/heads/main\n",
		".gitignore":        "/sub/hidden.txt\n/root-only.txt\n",
		"sub/hidden.txt":    "ignored by git\n",
		"sub/root-only.txt": "visible inside sub\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	info, err := Scan(filepath.Join(root, "sub"), 100)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]bool{}
	for _, entry := range info.Files {
		if !entry.IsDir {
			files[entry.RelPath] = true
		}
	}
	if files["hidden.txt"] {
		t.Error("repo-root ignore /sub/hidden.txt was not applied to subdirectory workspace")
	}
	if !files["root-only.txt"] {
		t.Error("repo-root ignore /root-only.txt incorrectly hides sub/root-only.txt")
	}
}

type cancelScanAfterChecks struct {
	context.Context
	remaining int
}

func (c *cancelScanAfterChecks) Err() error {
	c.remaining--
	if c.remaining <= 0 {
		return context.Canceled
	}
	return nil
}

func TestInventoryCancellationDuringNonGitScan(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 100; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprint(i)), []byte("file"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, after := range []int{1, 30} {
		ctx := &cancelScanAfterChecks{Context: context.Background(), remaining: after}
		if paths, err := Inventory(ctx, root, sandbox.New(root)); !errors.Is(err, context.Canceled) {
			t.Fatalf("after%d: %v, %v", after, paths, err)
		}
	}
}
