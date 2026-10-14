package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicationErrorsDescribeVisibleDiskState(t *testing.T) {
	writers := []struct {
		name  string
		write func(string, SyncFile, func(string) error) error
	}{
		{"bytes", func(path string, syncFile SyncFile, syncDir func(string) error) error {
			return WriteFileWithSync(path, []byte("new"), 0600, syncFile, syncDir)
		}},
		{"reader", func(path string, syncFile SyncFile, syncDir func(string) error) error {
			_, err := WriteReaderWithSync(path, strings.NewReader("new"), 0600, 10, syncFile, syncDir)
			return err
		}},
	}
	for _, writer := range writers {
		for _, afterPublish := range []bool{false, true} {
			stage := "file-sync"
			if afterPublish {
				stage = "directory-sync"
			}
			t.Run(writer.name+"/"+stage, func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "state")
				if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
					t.Fatal(err)
				}
				failure := errors.New("injected sync failure")
				var syncFile SyncFile
				var syncDir func(string) error
				if afterPublish {
					syncDir = func(string) error { return failure }
				} else {
					syncFile = func(*os.File) error { return failure }
				}
				err := writer.write(path, syncFile, syncDir)
				if !errors.Is(err, failure) {
					t.Fatalf("lost sync failure: %v", err)
				}
				var published *PublishedError
				if got := errors.As(err, &published); got != afterPublish {
					t.Fatalf("PublishedError=%v, want %v: %v", got, afterPublish, err)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				want := "old"
				if afterPublish {
					want = "new"
				}
				if string(data) != want {
					t.Fatalf("disk contains %q, want %q", data, want)
				}
				entries, err := os.ReadDir(root)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 1 || entries[0].Name() != "state" {
					t.Fatalf("publication left temporary files: %v", entries)
				}
			})
		}
	}
}
