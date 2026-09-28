package tools

import (
	"ccdp/internal/fsops"
	"ccdp/internal/sandbox"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func freshCtx(t *testing.T) *Context {
	t.Helper()
	dir := t.TempDir()
	return scopedTestContext(t, dir)
}

func runTool(t *testing.T, tool Tool, ctx *Context, args map[string]any) (string, error) {
	t.Helper()
	c := *ctx
	c.Args = args
	return tool.Run(&c)
}

func TestRecentReadsRecencyOrder(t *testing.T) {
	ctx := freshCtx(t)
	a := filepath.Join(t.TempDir(), "a.txt")
	b := filepath.Join(t.TempDir(), "b.txt")
	c := filepath.Join(t.TempDir(), "c.txt")
	for _, p := range []string{a, b, c} {
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		v, err := fsops.Observe(p)
		if err != nil {
			t.Fatal(err)
		}
		ctx.Resources.Files.MarkVersion(p, v)
	}

	// Newest first, bounded by n.
	recs := ctx.Resources.Files.RecentReads(2)
	if len(recs) != 2 || recs[0].Path != c || recs[1].Path != b {
		t.Fatalf("RecentReads(2) = %v", recs)
	}

	// Re-reading moves a path to the front of the recency order.
	v, err := fsops.Observe(a)
	if err != nil {
		t.Fatal(err)
	}
	ctx.Resources.Files.MarkVersion(a, v)
	recs = ctx.Resources.Files.RecentReads(3)
	if recs[0].Path != a || recs[2].Path != b {
		t.Fatalf("re-read did not refresh recency: %v", recs)
	}

	// Forgetting drops the path entirely.
	ctx.Resources.Files.ForgetFile(c)
	recs = ctx.Resources.Files.RecentReads(10)
	if len(recs) != 2 {
		t.Fatalf("forgotten path still tracked: %v", recs)
	}
	for _, r := range recs {
		if r.Path == c {
			t.Fatal("forgotten path returned by RecentReads")
		}
	}
}

func TestWriteFileNoFollowRejectsHardlinkDescriptor(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "protected.json")
	alias := filepath.Join(dir, "alias.json")
	original := []byte("protected\n")
	if err := os.WriteFile(target, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, alias); err != nil {
		t.Fatal(err)
	}
	v, _ := fsops.Observe(alias)
	if _, err := fsops.Publish(alias, []byte("attacker\n"), 0o644, &v); err == nil {
		t.Fatal("write through a multiply-linked alias was accepted")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("hardlink target changed after rejected write: %q", got)
	}
}

func TestStrictReadAllowsInternalSymlinkWithCanonicalOpen(t *testing.T) {
	ctx := freshCtx(t)
	ctx.Sandbox = sandbox.New(ctx.WorkingDir)
	target := filepath.Join(ctx.WorkingDir, "target.txt")
	alias := filepath.Join(ctx.WorkingDir, "alias.txt")
	if err := os.WriteFile(target, []byte("inside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	out, err := runTool(t, NewReadTool(), ctx, map[string]any{"file_path": alias})
	if err != nil {
		t.Fatalf("strict internal symlink read failed: %v", err)
	}
	if !strings.Contains(out, "inside") {
		t.Fatalf("strict symlink read omitted target content: %q", out)
	}
}
