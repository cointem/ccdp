package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func reviewGrepWrite(t *testing.T, root, path, body string) {
	t.Helper()
	p := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}
func TestFallbackGrepNestedIgnore(t *testing.T) {
	root := t.TempDir()
	reviewGrepWrite(t, root, "a/.gitignore", "skip.txt\n")
	reviewGrepWrite(t, root, "a/deep/skip.txt", "needle\n")
	reviewGrepWrite(t, root, "b/a/skip.txt", "needle\n")
	lines, _, err := inProcessSearch(grepCtx(root), &grepRequest{pattern: "needle", base: root})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(resultLocations(lines), " ")
	if got != "b/a/skip.txt:1" {
		t.Errorf("wrong nested gitignore scope: %q", got)
	}
}
func TestFallbackGrepMultilineCRLF(t *testing.T) {
	root := t.TempDir()
	reviewGrepWrite(t, root, "crlf.txt", "aaa\r\nbbb\r\nccc\r\nddd\r\n")
	lines, _, err := inProcessSearch(grepCtx(root), &grepRequest{pattern: "ccc\\r\\n", base: root, multiline: true})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(resultLocations(lines), " ")
	if got != "crlf.txt:3" {
		t.Errorf("CRLF multiline match belongs to line 3: %q", got)
	}
}
func TestFallbackGrepHonorsCancelledContext(t *testing.T) {
	root := t.TempDir()
	reviewGrepWrite(t, root, "file.txt", "needle\n")
	ctx := grepCtx(root)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	ctx.Context = cancelled
	if _, _, err := inProcessSearch(ctx, &grepRequest{pattern: "needle", base: root}); err == nil {
		t.Error("cancelled in-process search still returns success")
	}
}

func TestFallbackIgnoreFIFOIsSkipped(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, ".gitignore"), 0600); err != nil {
		t.Fatal(err)
	}
	reviewGrepWrite(t, root, "file.txt", "needle")
	done := make(chan error, 1)
	go func() {
		lines, _, err := inProcessSearch(grepCtx(root), &grepRequest{pattern: "needle", base: root})
		if err == nil && len(lines) != 1 {
			err = fmt.Errorf("got %d matches", len(lines))
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO ignore file blocked search")
	}
}

func TestFallbackIgnoreRulesAgreeWithRipgrep(t *testing.T) {
	rg, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("ripgrep unavailable")
	}
	root := t.TempDir()
	for path, body := range map[string]string{
		".gitignore":      "*.log\n!keep.log\n/only.txt\ndeep/**/drop*.txt\n",
		"a/.gitignore":    "skip*.txt\n!keep.txt\n/top.txt\ncache/\n",
		"a/deep/skip.txt": "needle", "b/a/skip.txt": "needle", "a/keep.txt": "needle", "a/top.txt": "needle", "a/deep/top.txt": "needle", "a/cache/file.txt": "needle", "only.txt": "needle", "sub/only.txt": "needle", "lost.log": "needle", "keep.log": "needle", "deep/drop.txt": "needle", "deep/x/y/drop2.txt": "needle", "deep/x/keep.txt": "needle",
	} {
		reviewGrepWrite(t, root, path, body)
	}
	output, err := exec.Command(rg, "--no-require-git", "--files-with-matches", "--sort", "path", "needle", root).Output()
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, path := range strings.Fields(string(output)) {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, filepath.ToSlash(rel))
	}
	lines, _, err := inProcessSearch(grepCtx(root), &grepRequest{pattern: "needle", base: root, outputMode: grepModeFiles})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, line := range lines {
		got = append(got, line.file)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fallback files %v differ from ripgrep %v", got, want)
	}
}

func TestGrepSubdirectoryInheritsOnlyScopedAncestors(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	for path, body := range map[string]string{
		".gitignore":                  "*.txt\n", // outside the authorized workspace ignore scope
		"workspace/.gitignore":        "/sub/hidden.txt\n/root-only.txt\n",
		"workspace/sub/hidden.txt":    "needle",
		"workspace/sub/root-only.txt": "needle",
		"workspace/sub/deep/keep.txt": "needle",
	} {
		reviewGrepWrite(t, parent, path, body)
	}
	ctx := grepCtx(root)
	req := &grepRequest{pattern: "needle", base: filepath.Join(root, "sub"), outputMode: grepModeFiles}
	for _, search := range []func(*Context, *grepRequest) ([]grepLine, bool, error){inProcessSearch} {
		lines, _, err := search(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		got := strings.Join(resultLocations(lines), " ")
		if got != "deep/keep.txt:0 root-only.txt:0" {
			t.Fatalf("wrong scoped ancestor rules: %q", got)
		}
	}
	// Snapshot boundary prevents live workspace ancestor rules from being read.
	ctx.ReadRoot = filepath.Join(root, "sub")
	lines, _, err := inProcessSearch(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 {
		t.Fatalf("snapshot search read outer workspace ignore rules: %+v", lines)
	}
}

func TestGrepSymlinkScopeUsesCanonicalDepth(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	for path, body := range map[string]string{
		".gitignore":                "*.txt\n",
		"workspace/.gitignore":      "/a/b/hidden.txt\n",
		"workspace/a/b/hidden.txt":  "needle",
		"workspace/a/b/visible.txt": "needle",
	} {
		reviewGrepWrite(t, parent, path, body)
	}
	if err := os.Symlink(filepath.Join(root, "a", "b"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	lines, _, err := inProcessSearch(grepCtx(root), &grepRequest{pattern: "needle", base: filepath.Join(root, "link"), outputMode: grepModeFiles})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(resultLocations(lines), " "); got != "visible.txt:0" {
		t.Fatalf("symlink scope inherited outside rules: %q", got)
	}
}

func TestSnapshotGrepRejectsImplicitIgnoreOutsideReadRoot(t *testing.T) {
	root := t.TempDir()
	snapshot := filepath.Join(root, "snapshot")
	reviewGrepWrite(t, root, "outside-ignore", "*.txt\n")
	reviewGrepWrite(t, root, "snapshot/visible.txt", "needle")
	if err := os.Symlink(filepath.Join(root, "outside-ignore"), filepath.Join(snapshot, ".gitignore")); err != nil {
		t.Fatal(err)
	}
	ctx := grepCtx(root)
	ctx.ReadRoot = snapshot
	req := &grepRequest{pattern: "needle", base: snapshot, outputMode: grepModeFiles}
	for _, search := range []func(*Context, *grepRequest) ([]grepLine, bool, error){inProcessSearch} {
		lines, _, err := search(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(resultLocations(lines), " "); got != "visible.txt:0" {
			t.Fatalf("applied snapshot-external ignore rules: %q", got)
		}
	}
}
