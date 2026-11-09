package tools

import (
	"ccdp/internal/fsops"
	"ccdp/internal/sandbox"
	"context"
	"fmt"
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
	return executeToolText(tool, &c)
}

func TestFileObservationsSurviveLargeReadSets(t *testing.T) {
	state := NewFileState("session")
	for i := 0; i < 1000; i++ {
		state.MarkVersion(fmt.Sprint(i), fsops.Version{Size: int64(i)})
	}
	if v, err := state.ObservedVersion("0"); err != nil || v.Size != 0 {
		t.Fatalf("old observation evicted: %v %v", v, err)
	}
	state.MarkVersion("0", fsops.Version{Size: 42})
	if v, err := state.ObservedVersion("0"); err != nil || v.Size != 42 {
		t.Fatal(v, err)
	}
	state.Clear()
	if _, err := state.ObservedVersion("999"); err == nil {
		t.Fatal("cleared observation retained")
	}
	state.Close()
	state.MarkVersion("new", fsops.Version{})
	if _, err := state.ObservedVersion("new"); err == nil {
		t.Fatal("closed state accepted observations")
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
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	v, _ := fsops.Observe(alias)
	if _, err := fsops.PublishAt(context.Background(), root, filepath.Base(alias), []byte("attacker\n"), 0o644, &v); err == nil {
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
	ctx.Sandbox = ctx.Resources.Context(ctx.Context, ctx.WorkingDir, sandbox.New(ctx.WorkingDir)).Sandbox
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
