package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArtifactRangeRevalidatesChangedFile(t *testing.T) {
	store, err := NewArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put([]byte(strings.Repeat("abcdef", 10000)))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		page, err := store.ReadRange(ref, 2, 3)
		if err != nil || string(page) != "cde" {
			t.Fatalf("range: %q, %v", page, err)
		}
	}
	if data, err := store.ReadRange(ref, ref.Size, 16); err != nil || len(data) != 0 {
		t.Fatalf("EOF: %q, %v", data, err)
	}
	if _, err := store.ReadRange(ref, ref.Size+1, 16); err == nil {
		t.Fatal("accepted an invalid range")
	}
	path := filepath.Join(store.Dir(), ref.Hash)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("ABCDEF", 10000)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadRange(ref, 2, 3); err == nil {
		t.Fatal("cached verification allowed a same-size edit with restored mtime")
	}
}
