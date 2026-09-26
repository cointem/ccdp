package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccdp/internal/sandbox"
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

func TestEditRequiresFreshRead(t *testing.T) {
	ctx := freshCtx(t)
	path := filepath.Join(ctx.WorkingDir, "a.txt")
	if err := os.WriteFile(path, []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Editing without a prior Read is rejected (read-before-edit gate).
	if _, err := runTool(t, NewEditTool(), ctx, map[string]any{
		"file_path": path, "old_string": "world", "new_string": "there",
	}); err == nil || !strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("expected not-read error, got %v", err)
	}

	// After a Read, the edit succeeds.
	if _, err := runTool(t, NewReadTool(), ctx, map[string]any{"file_path": path}); err != nil {
		t.Fatal(err)
	}
	out, err := runTool(t, NewEditTool(), ctx, map[string]any{
		"file_path": path, "old_string": "world", "new_string": "there",
	})
	if err != nil || !strings.Contains(out, "+ there") {
		t.Fatalf("edit after read failed: %q, %v", out, err)
	}
}

func TestEditDetectsExternalModification(t *testing.T) {
	ctx := freshCtx(t)
	path := filepath.Join(ctx.WorkingDir, "b.txt")
	if err := os.WriteFile(path, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(t, NewReadTool(), ctx, map[string]any{"file_path": path}); err != nil {
		t.Fatal(err)
	}

	// External modification after the Read (user, linter…).
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte("externally changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := runTool(t, NewEditTool(), ctx, map[string]any{
		"file_path": path, "old_string": "one", "new_string": "two",
	})
	if err == nil || !strings.Contains(err.Error(), "modified since") {
		t.Fatalf("expected stale-read rejection, got %v", err)
	}

	// Re-reading clears the staleness and the edit goes through.
	if _, err := runTool(t, NewReadTool(), ctx, map[string]any{"file_path": path}); err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(t, NewEditTool(), ctx, map[string]any{
		"file_path": path, "old_string": "externally changed", "new_string": "one",
	}); err != nil {
		t.Fatalf("edit after re-read failed: %v", err)
	}
}

func TestWriteOverwriteRequiresRead(t *testing.T) {
	ctx := freshCtx(t)
	path := filepath.Join(ctx.WorkingDir, "c.txt")
	if err := os.WriteFile(path, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Overwriting an existing unseen file is rejected.
	if _, err := runTool(t, NewWriteTool(), ctx, map[string]any{
		"file_path": path, "content": "new\n",
	}); err == nil || !strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("expected not-read rejection for overwrite, got %v", err)
	}

	// Creating a brand-new file needs no Read.
	newPath := filepath.Join(ctx.WorkingDir, "new.txt")
	if _, err := runTool(t, NewWriteTool(), ctx, map[string]any{
		"file_path": newPath, "content": "brand new\n",
	}); err != nil {
		t.Fatalf("new-file write should not require a read: %v", err)
	}

	// After a Read, overwriting succeeds.
	if _, err := runTool(t, NewReadTool(), ctx, map[string]any{"file_path": path}); err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(t, NewWriteTool(), ctx, map[string]any{
		"file_path": path, "content": "new\n",
	}); err != nil {
		t.Fatalf("overwrite after read failed: %v", err)
	}
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
		ctx.Resources.Files.MarkFileRead(p)
	}

	// Newest first, bounded by n.
	recs := ctx.Resources.Files.RecentReads(2)
	if len(recs) != 2 || recs[0].Path != c || recs[1].Path != b {
		t.Fatalf("RecentReads(2) = %v", recs)
	}

	// Re-reading moves a path to the front of the recency order.
	ctx.Resources.Files.MarkFileRead(a)
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

func TestFreshnessDetectsSameSizeTimestampRestoration(t *testing.T) {
	ctx := freshCtx(t)
	path := filepath.Join(ctx.WorkingDir, "fingerprint.txt")
	original := []byte("alpha\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(t, NewReadTool(), ctx, map[string]any{"file_path": path}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Replace with different bytes of the same length, then restore the old
	// mtime. Stat-only freshness checks would incorrectly accept this edit.
	if err := os.WriteFile(path, []byte("omega\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	_, err = runTool(t, NewEditTool(), ctx, map[string]any{
		"file_path": path, "old_string": "alpha", "new_string": "beta",
	})
	if err == nil || !strings.Contains(err.Error(), "content changed") {
		t.Fatalf("expected content fingerprint rejection, got %v", err)
	}
}

func TestFreshnessRejectsSmallReadReplacedByHugeSparseFile(t *testing.T) {
	ctx := freshCtx(t)
	path := filepath.Join(ctx.WorkingDir, "grew.txt")
	if err := os.WriteFile(path, []byte("small\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(t, NewReadTool(), ctx, map[string]any{"file_path": path}); err != nil {
		t.Fatal(err)
	}

	// Keep the same path and inode but grow it far beyond the read record. The
	// freshness gate must reject on the first descriptor stat rather than hash
	// this replacement's contents.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	const hugeSize = int64(2 << 30)
	if err := f.Truncate(hugeSize); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := runTool(t, NewEditTool(), ctx, map[string]any{
		"file_path": path, "old_string": "small", "new_string": "changed",
	}); err == nil || !strings.Contains(err.Error(), "modified since") {
		t.Fatalf("expected immediate size-change rejection for sparse replacement, got %v", err)
	}
}

func TestPartialReadDoesNotClaimCompleteFingerprint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 1<<20)), 0o644); err != nil {
		t.Fatal(err)
	}
	data, snapshot, err := readRangeSnapshot(path, 0, 64*1024, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 64*1024 {
		t.Fatalf("read %d bytes, want bounded 65536", len(data))
	}
	if snapshot.Complete {
		t.Fatal("partial read was recorded as a complete file fingerprint")
	}

	ctx := freshCtx(t)
	ctxPath := filepath.Join(ctx.WorkingDir, "large.txt")
	if err := os.WriteFile(ctxPath, []byte(strings.Repeat("y", 1<<20)), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runTool(t, NewReadTool(), ctx, map[string]any{"file_path": ctxPath, "limit": 64 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "partial read") || !strings.Contains(out, "full-file Edit/Write") {
		t.Fatalf("partial-read warning was not explicit enough: %q", out[:min(len(out), 256)])
	}
	if _, err := runTool(t, NewEditTool(), ctx, map[string]any{
		"file_path": ctxPath, "old_string": "y", "new_string": "z",
	}); err == nil || !strings.Contains(err.Error(), "only partially read") {
		t.Fatalf("expected partial-read freshness rejection, got %v", err)
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
	if err := writeFileNoFollow(alias, []byte("attacker\n"), 0o644); err == nil {
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
