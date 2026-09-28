package agent

import (
	"ccdp/internal/session"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/protocol"
)

func codingFixture(t *testing.T) (*Agent, string) {
	t.Helper()
	root := t.TempDir()
	git(t, root, "init", "-q")
	git(t, root, "config", "user.name", "Test")
	git(t, root, "config", "user.email", "test@example.invalid")
	git(t, root, "config", "commit.gpgsign", "false")
	for _, p := range []string{"chosen.txt", "staged.txt", "unstaged.txt"} {
		if e := os.WriteFile(filepath.Join(root, p), []byte("base\n"), 0644); e != nil {
			t.Fatal(e)
		}
	}
	git(t, root, "add", ".")
	git(t, root, "commit", "-qm", "base")
	cfg := config.Default()
	cfg.Workspace = root
	cfg.SessionDir = t.TempDir()
	cfg.PermissionMode = "bypass"
	cfg.EnableGuardian = config.BoolPtr(false)
	cfg.EnableMemory = config.BoolPtr(false)
	cfg.MCPServers = nil
	a, e := New(&cfg, nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(a.Close)
	return a, root
}
func runCoding(t *testing.T, a *Agent, w protocol.WorkflowCommand) (string, error) {
	t.Helper()
	cmd := protocol.Command{ID: protocol.CommandID(nextRuntimeID("test")), SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandRunWorkflow, Workflow: &w}
	submitWorkflow(t, a, cmd)
	done := waitWorkflowCompleted(t, a, cmd.ID)
	r, e := a.ReadOperation(context.Background(), cmd.ID)
	if w.Kind == protocol.WorkflowReview && w.Action == "" {
		deadline := time.Now().Add(10 * time.Second)
		for e == nil {
			r, e = a.ReadReviewOperation(context.Background(), cmd.ID)
			if r.Complete || e != nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("review child did not settle")
			}
			time.Sleep(5 * time.Millisecond)
		}
		if e == nil && r.Status != "success" {
			return r.Output, &codingTestError{r.Error}
		}
	}
	if e != nil {
		return "", e
	}
	if done.Outcome != "success" {
		return r.Output, &codingTestError{done.Report}
	}
	return r.Output, nil
}

type codingTestError struct{ s string }

func (e *codingTestError) Error() string { return e.s }
func deliveryID(t *testing.T, out string) string {
	t.Helper()
	var v struct {
		ID string `json:"plan_id"`
	}
	if e := json.Unmarshal([]byte(out), &v); e != nil || v.ID == "" {
		t.Fatalf("missing plan: %s (%v)", out, e)
	}
	return v.ID
}

func TestDeliveryPreservesUnrelatedStagedAndUnstagedChanges(t *testing.T) {
	a, root := codingFixture(t)
	for _, p := range []string{"chosen.txt", "staged.txt", "unstaged.txt"} {
		if e := os.WriteFile(filepath.Join(root, p), []byte(p+" changed\n"), 0644); e != nil {
			t.Fatal(e)
		}
	}
	git(t, root, "add", "staged.txt")
	old := git(t, root, "rev-parse", "HEAD")
	out, e := runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowCommitPushPR, Action: "prepare", Message: "selected", Mode: "commit", Paths: []string{"chosen.txt"}})
	if e != nil {
		t.Fatal(e)
	}
	if got := git(t, root, "rev-parse", "HEAD"); got != old {
		t.Fatal("prepare committed")
	}
	id := deliveryID(t, out)
	out, e = runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowCommitPushPR, Action: "apply", ID: id})
	if e != nil {
		t.Fatalf("apply: %v (%s)", e, out)
	}
	if got := git(t, root, "show", "--format=", "--name-only", "HEAD"); strings.TrimSpace(got) != "chosen.txt" {
		t.Fatalf("unexpected commit: %q", got)
	}
	if got := git(t, root, "diff", "--cached", "--name-only"); strings.TrimSpace(got) != "staged.txt" {
		t.Fatalf("lost staged intent: %q", got)
	}
	if got := git(t, root, "diff", "--name-only"); strings.TrimSpace(got) != "unstaged.txt" {
		t.Fatalf("lost unstaged intent: %q", got)
	}
	head := git(t, root, "rev-parse", "HEAD")
	if _, e = runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowCommitPushPR, Action: "resume", ID: id}); e != nil {
		t.Fatal(e)
	}
	if git(t, root, "rev-parse", "HEAD") != head {
		t.Fatal("retry made duplicate commit")
	}
}

func TestDeliveryRejectsStaleSelectionAndFailedChecks(t *testing.T) {
	for _, which := range []string{"stale", "check"} {
		t.Run(which, func(t *testing.T) {
			a, root := codingFixture(t)
			_ = os.WriteFile(filepath.Join(root, "chosen.txt"), []byte("chosen\n"), 0644)
			if which == "check" {
				a.mu.Lock()
				a.cfg.Delivery.Checks = []config.DeliveryCheck{{Name: "required", Argv: []string{"/bin/sh", "-c", "exit 17"}}}
				a.mu.Unlock()
			}
			before := git(t, root, "rev-parse", "HEAD")
			indexBefore, e := os.ReadFile(filepath.Join(root, ".git", "index"))
			if e != nil {
				t.Fatal(e)
			}
			out, e := runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowCommitPushPR, Message: "selected", Mode: "commit", Paths: []string{"chosen.txt"}})
			if e != nil {
				t.Fatal(e)
			}
			if which == "stale" {
				_ = os.WriteFile(filepath.Join(root, "chosen.txt"), []byte("user edit after preview\n"), 0644)
			}
			_, e = runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowCommitPushPR, Action: "apply", ID: deliveryID(t, out)})
			if e == nil {
				t.Fatal("expected failure")
			}
			if git(t, root, "rev-parse", "HEAD") != before {
				t.Fatal("failed operation committed")
			}
			indexAfter, _ := os.ReadFile(filepath.Join(root, ".git", "index"))
			if string(indexBefore) != string(indexAfter) {
				t.Fatal("failed operation changed index")
			}
		})
	}
}

func TestDeliveryInterruptStopsBeforeCommit(t *testing.T) {
	a, root := codingFixture(t)
	_ = os.WriteFile(filepath.Join(root, "chosen.txt"), []byte("chosen\n"), 0644)
	a.mu.Lock()
	a.cfg.Delivery.Checks = []config.DeliveryCheck{{Name: "wait", Argv: []string{"/bin/sh", "-c", "sleep 30"}}}
	a.mu.Unlock()
	out, e := runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowCommitPushPR, Message: "selected", Mode: "commit", Paths: []string{"chosen.txt"}})
	if e != nil {
		t.Fatal(e)
	}
	before := git(t, root, "rev-parse", "HEAD")
	id := protocol.CommandID(nextRuntimeID("interrupt-test"))
	submitWorkflow(t, a, protocol.Command{ID: id, SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandRunWorkflow, Workflow: &protocol.WorkflowCommand{Kind: protocol.WorkflowCommitPushPR, Action: "apply", ID: deliveryID(t, out)}})
	time.Sleep(250 * time.Millisecond)
	_, e = a.Submit(context.Background(), protocol.Command{ID: protocol.CommandID(nextRuntimeID("cancel")), SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandInterrupt})
	if e != nil {
		t.Fatal(e)
	}
	done := waitWorkflowCompleted(t, a, id)
	if done.Outcome != "error" {
		t.Fatal("interrupted workflow succeeded")
	}
	if git(t, root, "rev-parse", "HEAD") != before {
		t.Fatal("interrupted workflow committed")
	}
}

func TestRecoveryPreservesUserChangesAndIsIdempotent(t *testing.T) {
	a, root := codingFixture(t)
	ctx := context.Background()
	path := filepath.Join(root, "chosen.txt")
	_ = os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0644)
	point, e := a.captureRecoveryPoint(ctx, "turn-1", "change first line")
	if e != nil {
		t.Fatal(e)
	}
	finish, e := a.beforeCodingWrite(path, nil)
	if e != nil {
		t.Fatal(e)
	}
	_ = os.WriteFile(path, []byte("agent\ntwo\nthree\n"), 0644)
	if e = finish(); e != nil {
		t.Fatal(e)
	}
	if e = a.sealRecoveryPoint(point); e != nil {
		t.Fatal(e)
	}
	_ = os.WriteFile(path, []byte("agent\ntwo\nuser\n"), 0644)
	out, e := runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowRestore, Action: "prepare", ID: point.ID, Mode: "both"})
	if e != nil {
		t.Fatal(e)
	}
	id := deliveryID(t, out)
	if _, e = runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowRestore, Action: "apply", ID: id}); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "one\ntwo\nuser\n" {
		t.Fatalf("user edit lost: %q", b)
	}
	_ = os.WriteFile(path, []byte("later work\n"), 0644)
	if _, e = runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowRestore, Action: "recover", ID: id}); e != nil {
		t.Fatal(e)
	}
	b, _ = os.ReadFile(path)
	if string(b) != "later work\n" {
		t.Fatal("retry rewound later work")
	}
}

func TestRecoveryOpaqueWritesRequireResolution(t *testing.T) {
	a, root := codingFixture(t)
	point, e := a.captureRecoveryPoint(context.Background(), "turn-1", "opaque")
	if e != nil {
		t.Fatal(e)
	}
	_ = os.WriteFile(filepath.Join(root, "chosen.txt"), []byte("external\n"), 0644)
	if e = a.sealRecoveryPoint(point); e != nil {
		t.Fatal(e)
	}
	current, e := a.captureCoding(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	plan, e := a.inverseMutationPlan(point, current, nil)
	if e != nil {
		t.Fatal(e)
	}
	if len(plan.Edits) != 1 || plan.Edits[0].Conflict == "" {
		t.Fatalf("unattributed write auto-restored: %+v", plan)
	}
	resolved, e := a.codingState().store.Resolve(plan, map[string]string{"chosen.txt": "target"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = a.codingState().store.Apply(context.Background(), resolved, a.sandbox); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(filepath.Join(root, "chosen.txt"))
	if string(b) != "base\n" {
		t.Fatal(string(b))
	}
}

func TestReviewPersistsReadableReportsWithoutJSONRequirement(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			answer := `{"findings":[{"severity":"high","title":"Return value changed","explanation":"Callers expecting zero now receive one.","path":"a.go","side":"new","line":2,"end_line":2,"evidence":"return 1"}],"reviewed":["a.go"],"summary":"one issue"}`
			if !valid {
				answer = "## 发现\n未发现具体问题。\n## 检查范围与限制\n只检查 a.go；未运行测试。"
			}
			f := &fakeLLM{script: []string{"tool:Read|file_path=a.go", "text:" + answer}}
			a, _ := newTestAgent(t, f)
			sub, err := a.Watch(context.Background(), protocol.Cursor{})
			if err != nil {
				t.Fatal(err)
			}
			defer sub.Close()
			root := a.cfg.Workspace
			git(t, root, "init", "-q")
			git(t, root, "config", "user.name", "Test")
			git(t, root, "config", "user.email", "test@example.invalid")
			_ = os.WriteFile(filepath.Join(root, "a.go"), []byte("package p\nfunc A() int { return 0 }\n"), 0644)
			git(t, root, "add", "a.go")
			git(t, root, "-c", "commit.gpgsign=false", "commit", "-qm", "base")
			_ = os.WriteFile(filepath.Join(root, "a.go"), []byte("package p\nfunc A() int { return 1 }\n"), 0644)
			before, _ := os.ReadFile(filepath.Join(root, ".git", "index"))
			out, e := runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowReview, Scope: "uncommitted"})
			if valid && e != nil {
				t.Fatalf("review: %v %s", e, out)
			}
			if e != nil || !strings.Contains(out, answer) || !strings.Contains(out, "用时") {
				t.Fatalf("missing Markdown report: %s %v", out, e)
			}
			children, err := a.Sessions().ListChildren(context.Background())
			if err != nil || len(children) != 1 {
				t.Fatalf("review directory: %v %v", children, err)
			}
			row := children[0]
			if row.Purpose != "review" || row.DeliveryPending || row.Run.WaitPolicy != "observe" {
				t.Fatalf("review not observer-owned: %+v", row)
			}
			if len(a.History()) != 0 {
				t.Fatal("review result leaked into parent model context")
			}
			parentView, err := a.Snapshot(context.Background())
			if err != nil || parentView.Busy {
				t.Fatalf("review blocked parent: %v %v", parentView.Busy, err)
			}
			for _, item := range parentView.Transcript {
				if item.Kind == "tool" || item.Kind == "thinking" || item.Kind == "assistant" {
					t.Fatal("review leaked into parent transcript")
				}
			}
			reader, err := a.Sessions().OpenReader(context.Background(), row.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			view, err := reader.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			tool, final := false, false
			for _, item := range view.Transcript {
				tool = tool || item.Kind == "tool" && item.Tool == "Read" && item.Status == "success"
				final = final || strings.HasPrefix(item.ID, "review-report-") && item.Text == out
			}
			if !tool || !final {
				t.Fatalf("child reader missing process/report: tool=%v final=%v", tool, final)
			}

			after, _ := os.ReadFile(filepath.Join(root, ".git", "index"))
			if string(before) != string(after) {
				t.Fatal("review changed index")
			}
			records, err := a.persistenceHandle().Read(session.Beginning)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, record := range records {
				if record.Event.Type() == session.EventTypeReviewRecorded {
					found = true
				}
			}
			if !found {
				t.Fatal("missing durable review")
			}
		})
	}
}

func TestReviewCancelledBeforeSnapshotHasVisibleResult(t *testing.T) {
	a, _ := codingFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, _, err := a.runReviewWith(ctx, protocol.Command{ID: "cancelled-review", Workflow: &protocol.WorkflowCommand{Kind: protocol.WorkflowReview, Scope: "uncommitted"}}, nil)
	if err == nil {
		t.Fatal("cancelled review succeeded")
	}
	if !strings.Contains(out, "审查准备失败") {
		t.Fatalf("missing cancellation explanation: %q", out)
	}
}

func TestWriteChildIsIsolatedAndExplicitlyMerged(t *testing.T) {
	f := &fakeLLM{script: []string{"tool:Read|file_path=a.txt", "tool:Write|file_path=a.txt;mode=replace;content=child change", "text:done"}}
	a, _ := newTestAgent(t, f)
	root := a.cfg.Workspace
	git(t, root, "init", "-q")
	git(t, root, "config", "user.name", "Test")
	git(t, root, "config", "user.email", "test@example.invalid")
	_ = os.WriteFile(filepath.Join(root, "a.txt"), []byte("base"), 0644)
	git(t, root, "add", "a.txt")
	git(t, root, "-c", "commit.gpgsign=false", "commit", "-qm", "base")
	results, e := runTestChildren(a, []childTask{{Description: "change a.txt", Role: "worker", WorkspaceMode: "isolated"}}, "isolation-test")
	if e != nil {
		t.Fatal(e)
	}
	if len(results) != 1 || results[0].Error != "" {
		t.Fatalf("child: %+v", results)
	}
	b, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	if string(b) != "base" {
		t.Fatal("child changed parent directly")
	}
	rows, e := a.Sessions().ListChildren(context.Background())
	if e != nil || len(rows) != 1 {
		t.Fatalf("children: %+v %v", rows, e)
	}
	row := rows[0]
	if row.ResultSnapshotID == "" || row.Workspace == root {
		t.Fatalf("missing isolated snapshot: %+v", row)
	}
	t.Cleanup(func() {
		cmd := exec.Command("git", "worktree", "remove", "--force", row.Workspace)
		cmd.Dir = root
		_ = cmd.Run()
	})
	b, _ = os.ReadFile(filepath.Join(row.Workspace, "a.txt"))
	if string(b) != "child change" {
		t.Fatalf("child did not write: %q", b)
	}
	out, e := runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowMerge, Action: "prepare", ID: string(row.SessionID)})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowMerge, Action: "apply", ID: deliveryID(t, out)}); e != nil {
		t.Fatal(e)
	}
	b, _ = os.ReadFile(filepath.Join(root, "a.txt"))
	if string(b) != "child change" {
		t.Fatal("merge missing")
	}
	if _, err := os.Stat(row.Workspace); !os.IsNotExist(err) {
		t.Fatal("merged child workspace retained", err)
	}
	if listing := git(t, root, "worktree", "list", "--porcelain"); strings.Contains(listing, filepath.Base(row.Workspace)) {
		t.Fatal("merged worktree registration retained", listing)
	}
}

func TestDeliveryCreatesInitialCommitWithoutIndex(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init", "-q")
	git(t, root, "config", "user.name", "Test")
	git(t, root, "config", "user.email", "test@example.invalid")
	git(t, root, "config", "commit.gpgsign", "false")
	_ = os.WriteFile(filepath.Join(root, "first.txt"), []byte("first\n"), 0644)
	cfg := config.Default()
	cfg.Workspace = root
	cfg.SessionDir = t.TempDir()
	cfg.PermissionMode = "bypass"
	cfg.EnableGuardian = config.BoolPtr(false)
	a, e := New(&cfg, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	out, e := runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowCommitPushPR, Message: "initial", Mode: "commit", Paths: []string{"first.txt"}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowCommitPushPR, Action: "apply", ID: deliveryID(t, out)}); e != nil {
		t.Fatal(e)
	}
	if got := git(t, root, "show", "HEAD:first.txt"); got != "first\n" {
		t.Fatal(got)
	}
}

func TestPreparedRestoreSurvivesSessionRestart(t *testing.T) {
	a, root := codingFixture(t)
	point, e := a.captureRecoveryPoint(context.Background(), "turn-1", "before")
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(root, "chosen.txt")
	finish, e := a.beforeCodingWrite(path, nil)
	if e != nil {
		t.Fatal(e)
	}
	_ = os.WriteFile(path, []byte("agent\n"), 0644)
	if e = finish(); e != nil {
		t.Fatal(e)
	}
	if e = a.sealRecoveryPoint(point); e != nil {
		t.Fatal(e)
	}
	out, e := runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowRestore, Action: "prepare", ID: point.ID, Mode: "both"})
	if e != nil {
		t.Fatal(e)
	}
	id := deliveryID(t, out)
	cfg := a.cfg.Clone()
	sid := a.SessionID()
	a.Close()
	snap, e := LoadSession(cfg.SessionDir, sid)
	if e != nil {
		t.Fatal(e)
	}
	resumed, e := Resume(&cfg, snap, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer resumed.Close()
	if _, e = runCoding(t, resumed, protocol.WorkflowCommand{Kind: protocol.WorkflowRestore, Action: "apply", ID: id}); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "base\n" {
		t.Fatalf("restart recovery: %q", b)
	}
}

func TestDeliveryResumeAfterNetworkDenialDoesNotCommitAgain(t *testing.T) {
	a, root := codingFixture(t)
	remote := t.TempDir()
	git(t, remote, "init", "--bare", "-q")
	git(t, root, "remote", "add", "origin", remote)
	_ = os.WriteFile(filepath.Join(root, "chosen.txt"), []byte("chosen\n"), 0644)
	out, e := runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowCommitPushPR, Message: "ship", Mode: "push", Remote: "origin", Paths: []string{"chosen.txt"}})
	if e != nil {
		t.Fatal(e)
	}
	id := deliveryID(t, out)
	a.sandbox.SetAllowNetwork(false)
	_, e = runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowCommitPushPR, Action: "apply", ID: id})
	if e == nil {
		t.Fatal("push bypassed network denial")
	}
	commit := strings.TrimSpace(git(t, root, "rev-parse", "HEAD"))
	status, e := a.deliveryStatus(id)
	if e != nil || status.Stage != "committed" {
		t.Fatalf("local commit not retained: %+v %v", status, e)
	}
	a.sandbox.SetAllowNetwork(true)
	a.sandbox.AddDir(remote)
	if _, e = runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowCommitPushPR, Action: "resume", ID: id}); e != nil {
		t.Fatal(e)
	}
	if strings.TrimSpace(git(t, root, "rev-parse", "HEAD")) != commit {
		t.Fatal("resume committed again")
	}
	branch := strings.TrimSpace(git(t, root, "symbolic-ref", "HEAD"))
	if strings.TrimSpace(git(t, remote, "rev-parse", branch)) != commit {
		t.Fatal("resume did not push exact candidate")
	}
}
