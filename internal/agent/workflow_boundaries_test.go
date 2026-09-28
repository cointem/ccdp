package agent

import (
	"ccdp/internal/config"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/tools"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegressionSelectedOpaqueRewind(t *testing.T) {
	a, root := codingFixture(t)
	point, err := a.captureRecoveryPoint(context.Background(), "turn-audit", "before")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "chosen.txt"), []byte("external\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err = a.sealRecoveryPoint(point); err != nil {
		t.Fatal(err)
	}
	current, err := a.captureCoding(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	full, err := a.inverseMutationPlan(point, current, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Edits) != 1 || full.Edits[0].Conflict == "" {
		t.Fatalf("missing full conflict: %+v", full)
	}
	selected, err := a.inverseMutationPlan(point, current, []string{"chosen.txt"})
	if err != nil {
		t.Fatalf("selected conflict must remain resolvable: %v", err)
	}
	if len(selected.Edits) != 1 || selected.Edits[0].Conflict == "" {
		t.Fatalf("missing selected conflict: %+v", selected)
	}
}

func TestRegressionGitNonExecutablePermissions(t *testing.T) {
	a, root := codingFixture(t)
	if err := os.Chmod(filepath.Join(root, "chosen.txt"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(git(t, root, "status", "--porcelain")); got != "" {
		t.Fatalf("git is not clean: %s", got)
	}
	out, err := runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowCommitPushPR, Action: "prepare", Mode: "commit", Scope: "all", Message: "no changes"})
	if err == nil {
		t.Fatalf("clean Git tree incorrectly produced plan: %s", out)
	}
	if !strings.Contains(err.Error(), "nothing selected") {
		t.Fatal(err)
	}
}

func TestRegressionDeliveryWithUnchangedSubmodule(t *testing.T) {
	a, root := codingFixture(t)
	head := strings.TrimSpace(git(t, root, "rev-parse", "HEAD"))
	git(t, root, "update-index", "--add", "--cacheinfo", "160000,"+head+",vendor")
	git(t, root, "commit", "-qm", "gitlink")
	if err := os.Mkdir(filepath.Join(root, "vendor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "chosen.txt"), []byte("changed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowCommitPushPR, Action: "prepare", Mode: "commit", Paths: []string{"chosen.txt"}, Message: "chosen only"})
	if err != nil {
		t.Fatalf("unchanged gitlink blocked unrelated delivery: %v", err)
	}
}

func TestRegressionCodeRestoreRetryPreservesNewConversation(t *testing.T) {
	a, root := codingFixture(t)
	ctx := context.Background()
	point, err := a.captureRecoveryPoint(ctx, "audit-point", "before")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "chosen.txt"), []byte("changed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowRestore, Action: "prepare", ID: point.ID, Mode: "code", Scope: "snapshot"})
	if err != nil {
		t.Fatal(err)
	}
	id := deliveryID(t, out)
	tc := a.resources.Context(ctx, root, a.sandbox)
	tc.Args = map[string]any{"command": "sleep 30", "yield_ms": 0}
	raw, err := tools.NewBashTool().Run(tc)
	if err != nil {
		t.Fatal(err)
	}
	var proc struct {
		ID int `json:"process_id"`
	}
	if err = json.Unmarshal([]byte(raw), &proc); err != nil {
		t.Fatal(err)
	}
	_, err = runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowRestore, Action: "apply", ID: id})
	if err == nil || !strings.Contains(err.Error(), "stop managed") {
		t.Fatalf("expected active process guard: %v", err)
	}
	if err = a.appendHistory(messages.Message{Role: messages.RoleUser, Content: "KEEP_NEW_CONVERSATION"}); err != nil {
		t.Fatal(err)
	}
	tc.Args = map[string]any{"action": "stop", "id": proc.ID}
	if _, err = tools.NewProcessTool().Run(tc); err != nil {
		t.Fatal(err)
	}
	if _, err = runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowRestore, Action: "apply", ID: id}); err != nil {
		t.Fatal(err)
	}
	for _, m := range a.History() {
		if m.Content == "KEEP_NEW_CONVERSATION" {
			return
		}
	}
	t.Fatal("code-only restore retry erased a later conversation message")
}

func TestRegressionReviewFromRepositorySubdirectory(t *testing.T) {
	a, root := codingFixture(t)
	sub := filepath.Join(root, "pkg")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sub, "a.go")
	if err := os.WriteFile(path, []byte("package a\n"), 0644); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", "pkg/a.go")
	git(t, root, "commit", "-qm", "package")
	if err := os.WriteFile(path, []byte("package b\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := a.cfg.Clone()
	cfg.Workspace = sub
	cfg.SessionDir = t.TempDir()
	cfg.SessionID = ""
	child, err := New(&cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	called := false
	out, _, err := child.runReviewWith(context.Background(), protocol.Command{ID: "audit-subdir", Workflow: &protocol.WorkflowCommand{Scope: "uncommitted"}}, func(context.Context, config.Config, Options, string) (string, error) {
		called = true
		return "reviewed", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatalf("modified file not reviewed from subdirectory: %s", out)
	}
}

func TestRegressionInterleavedReasoningNotPrematurelyCompleted(t *testing.T) {
	tr := &transcriptState{}
	ev := protocol.EventView{Kind: protocol.EventReasoning, TurnID: "1", Text: "first thought"}
	tr.event(ev)
	tr.event(protocol.EventView{Kind: protocol.EventStream, TurnID: "1", Text: "interim answer"})
	ev.Text = "later thought"
	tr.event(ev)
	got := tr.eventItem(ev)
	if got.Status != "streaming" {
		t.Fatalf("live reasoning has immutable completed status: %+v", got)
	}
}

func TestCodeRestoreRecoveryKeepsCurrentConversation(t *testing.T) {
	a, root := codingFixture(t)
	point, err := a.captureRecoveryPoint(context.Background(), "restore-retry", "before")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "chosen.txt")
	if err = os.WriteFile(path, []byte("changed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowRestore, Action: "prepare", ID: point.ID, Mode: "code", Scope: "snapshot"})
	if err != nil {
		t.Fatal(err)
	}
	id := deliveryID(t, out)
	if err = os.WriteFile(path, []byte("external\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowRestore, Action: "apply", ID: id}); err == nil {
		t.Fatal("stale plan applied")
	}
	if err = a.appendHistory(messages.Message{Role: messages.RoleUser, Content: "KEEP_LATER_MESSAGE"}); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("changed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err = runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowRestore, Action: "recover", ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	found, notices := false, 0
	for _, m := range a.History() {
		if m.Content == "KEEP_LATER_MESSAGE" {
			found = true
		}
		if m.ID == "restore-notice-"+id {
			notices++
		}
	}
	if !found || notices != 1 {
		t.Fatalf("found current message=%v, restore notices=%d", found, notices)
	}
}
