package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccdp/internal/protocol"
)

func TestDeliveryUsesGitFileModesForFinalValidation(t *testing.T) {
	for _, tc := range []struct {
		name             string
		mode             os.FileMode
		changeExecutable bool
	}{
		{"private-file", 0600, false},
		{"private-executable", 0700, false},
		{"executable-bit-changed-after-prepare", 0600, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, root := codingFixture(t)
			path := filepath.Join(root, "chosen.txt")
			if err := os.WriteFile(path, []byte("selected\n"), tc.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatal(err)
			}
			oldHead := git(t, root, "rev-parse", "HEAD")
			indexBefore, err := os.ReadFile(filepath.Join(root, ".git", "index"))
			if err != nil {
				t.Fatal(err)
			}
			out, err := runCoding(t, a, protocol.WorkflowCommand{
				Kind: protocol.WorkflowCommitPushPR, Action: "prepare", Message: "selected",
				Mode: "commit", Paths: []string{"chosen.txt"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if tc.changeExecutable {
				if err := os.Chmod(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			out, err = runCoding(t, a, protocol.WorkflowCommand{
				Kind: protocol.WorkflowCommitPushPR, Action: "apply", ID: deliveryID(t, out),
			})
			if tc.changeExecutable {
				if err == nil || !strings.Contains(err.Error(), "workspace changed") {
					t.Fatalf("changed executable bit did not produce a stale-plan error: %v", err)
				}
				indexAfter, readErr := os.ReadFile(filepath.Join(root, ".git", "index"))
				if readErr != nil || string(indexAfter) != string(indexBefore) || git(t, root, "rev-parse", "HEAD") != oldHead {
					t.Fatal("stale mode check changed the user index or branch", readErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("valid Git file mode rejected: %v (%s)", err, out)
			}
			if got := git(t, root, "show", "HEAD:chosen.txt"); got != "selected\n" {
				t.Fatalf("wrong committed content: %q", got)
			}
			if got := git(t, root, "status", "--porcelain"); got != "" {
				t.Fatalf("mode normalization left worktree changes: %q", got)
			}
		})
	}
}
