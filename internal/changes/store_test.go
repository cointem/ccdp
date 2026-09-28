package changes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"ccdp/internal/sandbox"
)

func TestUnchangedBlobReusedAndCorruptionDetectedOnRead(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	data := []byte("unchanged workspace content")
	f, err := s.ImportFile(data, 0644, "regular")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "blobs", f.Chunks[0])
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportFile(data, 0644, "regular"); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || before.ModTime() != after.ModTime() {
		t.Fatal("unchanged blob was rewritten")
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportFile(data, 0644, "regular"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Content(f); err == nil {
		t.Fatal("corrupt blob accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportFile(data, 0644, "regular"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Content(f); !os.IsNotExist(err) {
		t.Fatalf("missing blob accepted: %v", err)
	}
}

func TestMergeRetainsIndependentUserEdit(t *testing.T) {
	got, e := MergeText([]byte("a\nb\nc\n"), []byte("a\nb\nUSER\n"), []byte("AGENT\nb\nc\n"))
	if e != nil || string(got) != "AGENT\nb\nUSER\n" {
		t.Fatalf("merge=%q %v", got, e)
	}
	_, e = MergeText([]byte("a\n"), []byte("user\n"), []byte("agent\n"))
	if !errors.Is(e, ErrConflict) {
		t.Fatalf("overlap error=%v", e)
	}
}

func TestRestorePlanIsStaleSafeAndIdempotent(t *testing.T) {
	root := t.TempDir()
	s := New(t.TempDir())
	policy := sandbox.New(root)
	ctx := context.Background()
	path := filepath.Join(root, "a.txt")
	write := func(v string) {
		t.Helper()
		if e := os.WriteFile(path, []byte(v), 0644); e != nil {
			t.Fatal(e)
		}
	}
	capture := func() Snapshot {
		t.Helper()
		v, e := s.CapturePaths(ctx, root, []string{"a.txt"}, policy)
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	write("before\n")
	before := capture()
	write("agent\n")
	after := capture()
	plan, e := s.Prepare(after, before, after, nil)
	if e != nil {
		t.Fatal(e)
	}
	write("user\n")
	if _, e = s.Apply(ctx, plan, policy); !errors.Is(e, ErrStale) {
		t.Fatalf("expected stale plan, got %v", e)
	}
	write("agent\n")
	if _, e = s.Apply(ctx, plan, policy); e != nil {
		t.Fatal(e)
	}
	write("later user\n")
	if _, e = s.Apply(ctx, plan, policy); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "later user\n" {
		t.Fatalf("duplicate apply overwrote later edit: %q", b)
	}
}

func TestSnapshotSurvivesRestartAndDetectsCorruption(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	s := New(state)
	if e := os.WriteFile(filepath.Join(root, "a"), []byte("original"), 0644); e != nil {
		t.Fatal(e)
	}
	snap, e := s.CapturePaths(context.Background(), root, []string{"a"}, sandbox.New(root))
	if e != nil {
		t.Fatal(e)
	}
	s = New(state)
	loaded, e := s.LoadSnapshot(snap.ID)
	if e != nil {
		t.Fatal(e)
	}
	f := loaded.Files["a"]
	b, e := s.Content(f)
	if e != nil || string(b) != "original" {
		t.Fatalf("content=%q %v", b, e)
	}
	if e = os.WriteFile(filepath.Join(state, "blobs", f.Chunks[0]), []byte("corrupt"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Content(f); e == nil {
		t.Fatal("corrupt blob accepted")
	}
}

func TestPlanPreservesUnrelatedFilesAndRequiresConflictDecision(t *testing.T) {
	s := New("")
	baseFile, _ := s.ImportFile([]byte("old"), 0644, "regular")
	newFile, _ := s.ImportFile([]byte("new"), 0644, "regular")
	userFile, _ := s.ImportFile([]byte("user"), 0644, "regular")
	base := Snapshot{Files: map[string]File{"a": baseFile}}
	target := Snapshot{Files: map[string]File{"a": newFile}}
	current := Snapshot{Root: t.TempDir(), Files: map[string]File{"a": userFile, "extra": userFile}}
	p, e := s.Prepare(base, target, current, nil)
	if e != nil || len(p.Edits) != 1 || p.Edits[0].Conflict == "" {
		t.Fatalf("plan=%+v %v", p, e)
	}
}

func TestMaterializeRejectsSymlinkAncestorEscape(t *testing.T) {
	s := New("")
	root := t.TempDir()
	outside := t.TempDir()
	link, e := s.ImportFile([]byte(outside), 0777, "symlink")
	if e != nil {
		t.Fatal(e)
	}
	file, e := s.ImportFile([]byte("bad"), 0644, "regular")
	if e != nil {
		t.Fatal(e)
	}
	snap := Snapshot{Files: map[string]File{"a": link, "a/escape": file}}
	if e = s.Materialize(context.Background(), snap, root); e == nil {
		t.Fatal("followed snapshot symlink ancestor")
	}
	if _, e = os.Stat(filepath.Join(outside, "escape")); !os.IsNotExist(e) {
		t.Fatal("wrote outside destination")
	}
}

func TestHunkSelectionAndConflictResolution(t *testing.T) {
	s := New("")
	b, _ := s.ImportFile([]byte("one\ntwo\nthree\n"), 0644, "regular")
	n, _ := s.ImportFile([]byte("ONE\ntwo\nTHREE\n"), 0644, "regular")
	base := Snapshot{Files: map[string]File{"a": b}}
	target := Snapshot{Files: map[string]File{"a": n}}
	hunks, e := s.Hunks(base, target)
	if e != nil || len(hunks) != 2 {
		t.Fatalf("hunks: %+v %v", hunks, e)
	}
	selected, e := s.Select(base, target, nil, []string{hunks[0].ID})
	if e != nil {
		t.Fatal(e)
	}
	data, e := s.Content(selected.Files["a"])
	if e != nil || string(data) != "ONE\ntwo\nthree\n" {
		t.Fatalf("selection: %q %v", data, e)
	}
	if _, e = s.Select(base, target, nil, []string{"stale"}); e == nil {
		t.Fatal("accepted stale hunk")
	}
	p, _ := s.SavePlan(Plan{Edits: []Edit{{Path: "a", Before: b, After: n, Conflict: "overlap"}}})
	resolved, e := s.Resolve(p, map[string]string{"a": "target"})
	if e != nil || resolved.ID == p.ID || resolved.Edits[0].Conflict != "" {
		t.Fatalf("resolution: %+v %v", resolved, e)
	}
}

func TestWorkspaceLockSpansStores(t *testing.T) {
	root := t.TempDir()
	a, e := LockWorkspace(root)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	if b, e := LockWorkspace(root); e == nil {
		b.Close()
		t.Fatal("second publisher acquired workspace lock")
	}
}
