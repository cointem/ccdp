package workspace

import (
	"ccdp/internal/sandbox"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestLanguageQueryRejectsNonregularDocuments(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"fifo", "dir"} {
		_, err := LanguageQuery(context.Background(), root, name, "outline", 1, 0, LanguageServer{Argv: []string{"must-not-start"}, LanguageID: "go"}, sandbox.New(root))
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
