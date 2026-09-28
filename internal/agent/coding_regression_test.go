package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"ccdp/internal/config"
	"ccdp/internal/fsops"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func TestMergedWorkspaceRecoveryPreservesActiveAndChangedDirectories(t *testing.T) {
	a, _ := codingFixture(t)
	base := filepath.Join(filepath.Dir(a.cfg.SessionDir), "workspaces")
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, "ccdp-workspace-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "file")
	if err = os.WriteFile(path, []byte("saved"), 0600); err != nil {
		t.Fatal(err)
	}
	policy := a.sandbox.Snapshot()
	policy.AddReadOnlyDir(dir)
	snap, err := a.codingState().store.CapturePaths(context.Background(), dir, []string{"file"}, policy)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	close(done)
	r := &managedRun{done: done, fact: session.ChildRunRecorded{Child: protocol.ChildSession{SessionID: "recovery-child", Workspace: dir, MergeStatus: "merged", ResultSnapshotID: snap.ID, Run: protocol.RunView{Status: "succeeded"}}}}
	a.supervisor.mu.Lock()
	a.supervisor.children["recovery-child"] = r
	a.supervisor.mu.Unlock()
	t.Cleanup(func() {
		a.supervisor.mu.Lock()
		delete(a.supervisor.children, "recovery-child")
		a.supervisor.mu.Unlock()
	})
	lock, err := fsops.LeaseWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	a.supervisor.cleanupMergedWorkspaces()
	if _, err = os.Stat(path); err != nil {
		t.Fatal("removed active workspace", err)
	}
	lock.Close()
	if err = os.WriteFile(path, []byte("new user edit"), 0600); err != nil {
		t.Fatal(err)
	}
	a.supervisor.cleanupMergedWorkspaces()
	if _, err = os.Stat(path); err != nil {
		t.Fatal("removed newer edits", err)
	}
	if err = os.WriteFile(path, []byte("saved"), 0600); err != nil {
		t.Fatal(err)
	}
	a.supervisor.cleanupMergedWorkspaces()
	if _, err = os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("merged crash residue retained", err)
	}
}

func TestPartialChildMergeRetainsUnmergedWork(t *testing.T) {
	f := &fakeLLM{script: []string{"tool:Read|file_path=a.txt", "tool:Write|file_path=a.txt;mode=replace;content=child a", "tool:Read|file_path=b.txt", "tool:Write|file_path=b.txt;mode=replace;content=child b", "text:done"}}
	a, _ := newTestAgent(t, f)
	root := a.cfg.Workspace
	git(t, root, "init", "-q")
	git(t, root, "config", "user.name", "Test")
	git(t, root, "config", "user.email", "test@example.invalid")
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("base"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, root, "add", ".")
	git(t, root, "-c", "commit.gpgsign=false", "commit", "-qm", "base")
	results, err := runTestChildren(a, []childTask{{Description: "edit both files", Role: "worker", WorkspaceMode: "isolated"}}, "partial-merge")
	if err != nil || len(results) != 1 || results[0].Error != "" {
		t.Fatal(results, err)
	}
	rows, err := a.Sessions().ListChildren(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	row := rows[0]
	t.Cleanup(func() { _ = a.removeChildWorkspace(context.Background(), row.Workspace) })
	for i, paths := range [][]string{{"a.txt"}, nil} {
		out, err := runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowMerge, Action: "prepare", ID: string(row.SessionID), Paths: paths})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowMerge, Action: "apply", ID: deliveryID(t, out)}); err != nil {
			t.Fatal(err)
		}
		_, err = os.Stat(row.Workspace)
		if i == 0 && err != nil {
			t.Fatal("partial merge retired workspace", err)
		}
		if i == 1 && !os.IsNotExist(err) {
			t.Fatal("full merge retained workspace", err)
		}
	}
}

func TestChildCleanupRejectsUnownedDirectory(t *testing.T) {
	a, _ := codingFixture(t)
	dir := t.TempDir()
	if err := a.removeChildWorkspace(context.Background(), dir); err == nil {
		t.Fatal("accepted unowned directory")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("removed unowned directory", err)
	}
}

func TestChildCleanupRecoversMissingWorktreeDirectory(t *testing.T) {
	a, root := codingFixture(t)
	base := filepath.Join(filepath.Dir(a.cfg.SessionDir), "workspaces")
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, "ccdp-workspace-")
	if err != nil {
		t.Fatal(err)
	}
	git(t, root, "worktree", "add", "--detach", dir, "HEAD")
	// Simulate a crash between removing contents and Git administrative data.
	if err = os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err = a.removeChildWorkspace(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if listing := git(t, root, "worktree", "list", "--porcelain"); strings.Contains(listing, filepath.Base(dir)) {
		t.Fatal("orphan registration retained", listing)
	}
}

func TestDeliveryIndexLockRecoveryPreservesForeignLocks(t *testing.T) {
	index := filepath.Join(t.TempDir(), "index")
	lock, err := createDeliveryIndexLock(index, "plan")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = lock.WriteString("candidate index"); err != nil {
		t.Fatal(err)
	}
	lock.Close() // simulate the process exiting without cleanup
	if err = recoverDeliveryIndexLock(index, "plan"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(index + ".lock"); !os.IsNotExist(err) {
		t.Fatal("owned lock retained", err)
	}
	if err = os.WriteFile(index+".lock", []byte("other Git"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = recoverDeliveryIndexLock(index, "plan"); err != nil {
		t.Fatal(err)
	}
	if _, err = createDeliveryIndexLock(index, "plan"); err == nil {
		t.Fatal("replaced foreign lock")
	}
	b, err := os.ReadFile(index + ".lock")
	if err != nil || string(b) != "other Git" {
		t.Fatal("foreign lock changed", err)
	}
}

func TestDeliveryResumeReusesCandidate(t *testing.T) {
	a, root := codingFixture(t)
	a.mu.Lock()
	a.cfg.Delivery.Checks = []config.DeliveryCheck{{Name: "retry", Argv: []string{"sh", "-c", "test -f retry-marker || { touch retry-marker; exit 1; }"}}}
	a.mu.Unlock()
	if err := os.WriteFile(filepath.Join(root, "chosen.txt"), []byte("changed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowCommitPushPR, Action: "prepare", Message: "retry checks", Mode: "commit", Paths: []string{"chosen.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	id := deliveryID(t, out)
	_, err = runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowCommitPushPR, Action: "apply", ID: id})
	if err == nil {
		t.Fatal("expected check failure")
	}
	state, err := a.deliveryStatus(id)
	if err != nil || state.Stage != "checking" || state.Candidate == "" {
		t.Fatal(state, err)
	}
	candidate := state.Candidate
	t.Cleanup(func() { _ = os.RemoveAll(candidate) })
	if _, err = runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowCommitPushPR, Action: "resume", ID: id}); err != nil {
		t.Fatal(err)
	}
	state, err = a.deliveryStatus(id)
	if err != nil || state.Candidate != candidate {
		t.Fatal("candidate changed", state, err)
	}
	if _, err = os.Stat(candidate); !os.IsNotExist(err) {
		t.Fatal("committed candidate retained", err)
	}
}

func TestReviewSourceRejectsReplacedEntry(t *testing.T) {
	for _, replacement := range []string{"symlink", "fifo", "regular"} {
		t.Run(replacement, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "source")
			if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			// Keep the original inode alive so replacement cannot reuse it.
			if err = os.Rename(path, path+".old"); err != nil {
				t.Fatal(err)
			}
			switch replacement {
			case "symlink":
				err = os.Symlink(path+".old", path)
			case "fifo":
				err = syscall.Mkfifo(path, 0600)
			case "regular":
				err = os.WriteFile(path, []byte("replacement"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if b, err := readReviewSource(root, "source", info, 1024); err == nil || len(b) != 0 {
				t.Fatal("read replaced source", string(b), err)
			}
		})
	}
}

func TestReviewStageProgressDoesNotReplaceMainStatus(t *testing.T) {
	a, _ := codingFixture(t)
	a.mu.Lock()
	a.workLabel = "main task"
	a.mu.Unlock()
	a.reviewProgress(context.Background(), "review-progress", "preparing review")
	a.mu.Lock()
	label := a.workLabel
	a.mu.Unlock()
	if label != "main task" {
		t.Fatal("overwrote main task status")
	}
	a.transcript.mu.Lock()
	defer a.transcript.mu.Unlock()
	// The same snapshot projection used by watch reconnects contains progress.
	found := false
	for _, item := range a.transcript.items {
		if strings.HasPrefix(item.ID, "review:review-progress:") && item.Text == "preparing review" {
			found = true
		}
	}
	if !found {
		t.Fatal("progress absent from transcript")
	}
}
