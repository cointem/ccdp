package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"ccdp/internal/protocol"
	"ccdp/internal/workspace"
)

func gitStructureFixture(t *testing.T, toDirectory bool) (*Agent, string, string, string) {
	t.Helper()
	a, root := codingFixture(t)
	before, after := "node", "node/deep/child"
	if !toDirectory {
		before, after = after, before
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write(before, "before structure change\n")
	git(t, root, "add", before)
	git(t, root, "commit", "-qm", "base structure")
	if err := os.RemoveAll(filepath.Join(root, "node")); err != nil {
		t.Fatal(err)
	}
	write(after, "after structure change\n")
	return a, root, before, after
}

func TestGitStructureInventoryAndReview(t *testing.T) {
	for _, toDirectory := range []bool{false, true} {
		t.Run(map[bool]string{false: "directory-to-file", true: "file-to-directory"}[toDirectory], func(t *testing.T) {
			a, root, before, after := gitStructureFixture(t, toDirectory)
			paths, err := workspace.Inventory(context.Background(), root, a.services.sandbox)
			if err != nil {
				t.Fatal(err)
			}
			if slices.Contains(paths, before) || !slices.Contains(paths, after) {
				t.Fatalf("inventory kept stale leaves or lost replacement: %v", paths)
			}
			for _, scope := range []string{"uncommitted", "staged", "commit"} {
				t.Run(scope, func(t *testing.T) {
					if scope == "staged" {
						git(t, root, "add", "-A", "--", "node")
					}
					if scope == "commit" {
						git(t, root, "commit", "-qm", "replace structure")
					}
					w := &protocol.WorkflowCommand{Scope: scope, Commit: "HEAD"}
					copy, err := a.prepareReviewCopy(context.Background(), protocol.Command{ID: "structure-review", Workflow: w})
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = os.RemoveAll(copy.dir); copy.owner.Close() }()
					_, count, err := copy.task(context.Background(), w)
					if err != nil {
						t.Fatal(err)
					}
					if count != 2 {
						t.Fatalf("expected deletion and addition, got %d; exclusions=%v", count, copy.excluded)
					}
					data, err := os.ReadFile(filepath.Join(copy.dir, "changes.json"))
					if err != nil {
						t.Fatal(err)
					}
					var manifest struct {
						Changes []struct{ Path, Status string }
					}
					if err := json.Unmarshal(data, &manifest); err != nil {
						t.Fatal(err)
					}
					status := map[string]string{}
					for _, change := range manifest.Changes {
						status[change.Path] = change.Status
					}
					if status[before] != "deleted" || status[after] != "added" {
						t.Fatalf("incorrect structural diff: %s", data)
					}
					patch, err := os.ReadFile(filepath.Join(copy.dir, "changes.patch"))
					if err != nil || !strings.Contains(string(patch), "-before structure change") || !strings.Contains(string(patch), "+after structure change") {
						t.Fatalf("structural patch incomplete: %s (%v)", patch, err)
					}
				})
			}
		})
	}
}

func TestDeliveryFileDirectoryTransitions(t *testing.T) {
	for _, toDirectory := range []bool{false, true} {
		t.Run(map[bool]string{false: "directory-to-file", true: "file-to-directory"}[toDirectory], func(t *testing.T) {
			a, root, _, after := gitStructureFixture(t, toDirectory)
			out, err := runCoding(t, a, protocol.WorkflowCommand{
				Kind: protocol.WorkflowCommitPushPR, Action: "prepare", Message: "replace structure", Mode: "commit", Scope: "all",
			})
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			out, err = runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowCommitPushPR, Action: "apply", ID: deliveryID(t, out)})
			if err != nil {
				t.Fatalf("apply: %v (%s)", err, out)
			}
			if got := git(t, root, "show", "HEAD:"+after); got != "after structure change\n" {
				t.Fatalf("wrong committed replacement: %q", got)
			}
			if got := git(t, root, "status", "--porcelain"); got != "" {
				t.Fatalf("published index differs from the worktree: %q", got)
			}
		})
	}
}
