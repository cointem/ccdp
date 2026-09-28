package agent

import (
	"ccdp/internal/config"
	"ccdp/internal/protocol"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewSelectionAdmittedBeforeContextQuota(t *testing.T) {
	a, root := codingFixture(t)
	_ = os.WriteFile(filepath.Join(root, "chosen.txt"), []byte("changed\n"), 0644)
	_ = os.WriteFile(filepath.Join(root, "aaa-unselected"), []byte(strings.Repeat("x", 4096)), 0644)
	a.mu.Lock()
	a.cfg.Review = config.ReviewConfig{FileBytes: 1024, TotalBytes: 1024, MaxPaths: 1}
	a.mu.Unlock()
	cmd := protocol.Command{ID: "quota", Workflow: &protocol.WorkflowCommand{Scope: "uncommitted", Paths: []string{"chosen.txt"}}}
	r, err := a.prepareReviewCopy(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { os.RemoveAll(r.dir); r.owner.Close() }()
	_, count, err := r.task(context.Background(), cmd.Workflow)
	if err != nil || count != 1 || !r.retained["chosen.txt"] || r.omittedContext == 0 || r.excludedCount != 0 {
		t.Fatal(count, r.retained, r.excluded, err)
	}
	if r.total != int64(len("base\nchanged\n")) {
		t.Fatal("bytes counted before retention", r.total)
	}
}

func TestReviewPairExclusionNeverCreatesFalseDeletion(t *testing.T) {
	a, root := codingFixture(t)
	_ = os.WriteFile(filepath.Join(root, "chosen.txt"), []byte("binary\x00data"), 0644)
	cmd := protocol.Command{ID: "pair", Workflow: &protocol.WorkflowCommand{Scope: "uncommitted", Paths: []string{"chosen.txt"}}}
	r, err := a.prepareReviewCopy(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { os.RemoveAll(r.dir); r.owner.Close() }()
	_, count, err := r.task(context.Background(), cmd.Workflow)
	if err != nil || count != 0 {
		t.Fatal(count, err)
	}
	for _, side := range []string{"before", "after"} {
		if _, err = os.Stat(filepath.Join(r.dir, side, "chosen.txt")); !os.IsNotExist(err) {
			t.Fatal("half pair retained", err)
		}
	}
}

func TestReviewFilterExclusionDoesNotConsumeContextQuota(t *testing.T) {
	a, root := codingFixture(t)
	for path, data := range map[string]string{"chosen.txt": "filtered\n", ".gitattributes": "chosen.txt filter=custom\n", "context.txt": "context\n"} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	a.cfg.Review = config.ReviewConfig{FileBytes: 1024, TotalBytes: 1024, MaxPaths: 2}
	r, err := a.prepareReviewCopy(context.Background(), protocol.Command{ID: "filter-quota", Workflow: &protocol.WorkflowCommand{Scope: "uncommitted", Paths: []string{"chosen.txt"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { os.RemoveAll(r.dir); r.owner.Close() }()
	if r.retained["chosen.txt"] || !r.retained["context.txt"] || !strings.Contains(r.excluded["chosen.txt"], "filter") {
		t.Fatal(r.retained, r.excluded)
	}
}

func TestPartialReviewIsNotAnExecutionError(t *testing.T) {
	a, root := codingFixture(t)
	_ = os.WriteFile(filepath.Join(root, "chosen.txt"), []byte("binary\x00data"), 0644)
	out, outcome, err := a.runReviewWith(context.Background(), protocol.Command{ID: "partial", Workflow: &protocol.WorkflowCommand{Scope: "uncommitted", Paths: []string{"chosen.txt"}}}, nil)
	if err != nil || outcome != protocol.OutcomePartial || !strings.Contains(out, "未覆盖") {
		t.Fatal(outcome, err, out)
	}
}

func TestReportOnlyDoesNotHaveTools(t *testing.T) {
	a, _ := codingFixture(t)
	cfg, opts, err := childOptionsFromParent(a, childPurposeReview)
	if err != nil {
		t.Fatal(err)
	}
	bindReportOnly(&cfg, &opts)
	child, err := newAgentWithOptions(&cfg, nil, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	view, err := child.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(view.Catalog)
	if strings.Contains(string(b), `"name":"Bash"`) {
		t.Fatal("report-only tool restored", string(b))
	}
	if len(opts.AllowedTools) != 0 || opts.AllowedTools == nil {
		t.Fatal("missing explicit empty tool set")
	}
}
