package execution

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccdp/internal/executable"
	"ccdp/internal/sandbox"
)

func TestRgSnapshotProfileBlocksOutsideFilesAndSymlinks(t *testing.T) {
	if executable.FindTrusted("rg", os.Getenv("PATH")) == "" {
		t.Skip("rg unavailable")
	}
	root := t.TempDir()
	snapshot := filepath.Join(root, "snapshot")
	if err := os.Mkdir(snapshot, 0700); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(snapshot, "inside.txt")
	outside := filepath.Join(root, "outside.txt")
	for path, body := range map[string]string{inside: "needle inside", outside: "needle OUTSIDE_SECRET"} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(snapshot, "escape.txt")
	if err := os.Symlink(outside, alias); err != nil {
		t.Fatal(err)
	}
	policy := sandbox.New(root)
	for _, path := range []string{inside, outside, alias} {
		res, err := RunReadOnlySearch(context.Background(), []string{"--no-config", "--no-ignore", "needle", "--", path}, Request{Dir: snapshot, Sandbox: policy}, io.Discard, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if path == inside {
			if res.ExitCode != 0 || !strings.Contains(res.Stdout, "needle inside") {
				t.Fatalf("snapshot data inaccessible: %+v", res)
			}
		} else if res.ExitCode == 0 || strings.Contains(res.Stdout, "OUTSIDE_SECRET") {
			t.Fatalf("snapshot boundary bypassed by %s: %+v", path, res)
		}
	}
}
