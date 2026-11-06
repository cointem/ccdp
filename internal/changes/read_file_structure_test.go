package changes

import (
	"ccdp/internal/testenv"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestReadFileRecognizesAbsentLeavesAcrossStructureChanges(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "directory"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	store := New("")
	policy := testenv.Sandbox(t, root)
	for _, path := range []string{"missing", "directory", "file/old-child"} {
		got, err := store.ReadFile(root, path, policy)
		if err != nil || got.Kind != "absent" {
			t.Fatalf("%s: got %+v, err %v", path, got, err)
		}
	}
	// Structural publication still needs to distinguish a directory from an
	// absent leaf; only ReadFile has the file-manifest interpretation.
	got, err := store.planEntry(root, "directory", policy)
	if err != nil || got.Kind != "directory" {
		t.Fatalf("plan entry lost directory state: %+v (%v)", got, err)
	}
}

func TestReadFileDoesNotTreatDeniedOrUnsupportedPathsAsAbsent(t *testing.T) {
	root := t.TempDir()
	store := New("")
	policy := testenv.Sandbox(t, root)
	if err := os.Mkdir(filepath.Join(root, "denied"), 0700); err != nil {
		t.Fatal(err)
	}
	policy.AddDisallowedDir(filepath.Join(root, "denied"))
	if _, err := store.ReadFile(root, "denied", policy); err == nil {
		t.Fatal("denied directory was treated as an absent file")
	}
	if err := syscall.Mkfifo(filepath.Join(root, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadFile(root, "fifo", policy); err == nil {
		t.Fatal("unsupported FIFO was treated as an absent file")
	}
	if _, err := store.ReadFile(filepath.Join(root, "missing-root"), "file", policy); err == nil {
		t.Fatal("missing workspace was treated as an absent file")
	}
}
