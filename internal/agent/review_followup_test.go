package agent

import (
	"ccdp/internal/config"
	"ccdp/internal/protocol"
	"ccdp/internal/tools"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegressionCleanSymlinkReview(t *testing.T) {
	a, root := codingFixture(t)
	if err := os.Symlink("chosen.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", "link.txt")
	git(t, root, "commit", "-qm", "link")
	out, outcome, err := a.runReviewWith(context.Background(), protocol.Command{ID: "audit-link", Workflow: &protocol.WorkflowCommand{Scope: "uncommitted"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != protocol.OutcomeSuccess {
		t.Fatalf("clean repository got %s: %s", outcome, out)
	}
}

func TestRegressionSelectedReviewContextQuota(t *testing.T) {
	a, root := codingFixture(t)
	if err := os.WriteFile(filepath.Join(root, "chosen.txt"), []byte("changed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	a.cfg.Review = config.ReviewConfig{FileBytes: 1024, TotalBytes: 1024, MaxPaths: 1}
	out, outcome, err := a.runReviewWith(context.Background(), protocol.Command{ID: "audit-quota", Workflow: &protocol.WorkflowCommand{Scope: "uncommitted", Paths: []string{"chosen.txt"}}}, func(context.Context, config.Config, Options, string) (string, error) {
		return "selected file reviewed", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != protocol.OutcomeSuccess {
		t.Fatalf("fully reviewed selected file got %s: %s", outcome, out)
	}
}

func TestRegressionCheckCannotExpandDelivery(t *testing.T) {
	a, root := codingFixture(t)
	if err := os.WriteFile(filepath.Join(root, "chosen.txt"), []byte("chosen\n"), 0644); err != nil {
		t.Fatal(err)
	}
	a.cfg.Delivery.Checks = []config.DeliveryCheck{{Name: "stage-generated", Argv: []string{"/bin/sh", "-c", "printf 'generated\\n' > generated.txt && git add generated.txt"}}}
	head := git(t, root, "rev-parse", "HEAD")
	out, err := runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowCommitPushPR, Action: "prepare", Message: "chosen only", Mode: "commit", Paths: []string{"chosen.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	id := deliveryID(t, out)
	for _, action := range []string{"apply", "resume"} {
		_, err = runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowCommitPushPR, Action: action, ID: id})
		if err == nil || !strings.Contains(err.Error(), "checks changed candidate index") {
			t.Fatalf("%s should reject expanded index, got %v", action, err)
		}
		if got := git(t, root, "rev-parse", "HEAD"); got != head {
			t.Fatalf("%s changed HEAD: %s", action, got)
		}
	}
}

func TestRegressionNoopWriteDoesNotBlockRewind(t *testing.T) {
	a, root := codingFixture(t)
	ctx := context.Background()
	point, err := a.captureRecoveryPoint(ctx, "noop-turn", "same content")
	if err != nil {
		t.Fatal(err)
	}
	toolCtx := a.resources.Context(ctx, root, a.sandbox)
	toolCtx.BeforeWrite = a.beforeCodingWrite
	toolCtx.Args = map[string]any{"file_path": "chosen.txt"}
	if _, err = tools.NewReadTool().Run(toolCtx); err != nil {
		t.Fatal(err)
	}
	toolCtx.Args = map[string]any{"file_path": "chosen.txt", "content": "base\n", "mode": "replace"}
	if _, err = tools.NewWriteTool().Run(toolCtx); err != nil {
		t.Fatal(err)
	}
	if err = a.sealRecoveryPoint(point); err != nil {
		t.Fatal(err)
	}
	current, err := a.captureCoding(ctx)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := a.inverseMutationPlan(point, current, nil)
	if err != nil {
		t.Fatalf("no-op successful write blocks rewind: %v", err)
	}
	if len(plan.Edits) != 0 {
		t.Fatalf("no-op write produced edits: %+v", plan.Edits)
	}
}
