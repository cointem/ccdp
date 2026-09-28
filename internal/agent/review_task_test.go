package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccdp/internal/config"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func TestReviewTaskUsesOnDemandFilesInsteadOfInlineSource(t *testing.T) {
	a, root := codingFixture(t)
	large := "UNIQUE_SOURCE_SENTINEL" + strings.Repeat("x", 70<<10)
	if err := os.WriteFile(filepath.Join(root, "chosen.txt"), []byte(large), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "staged.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("added content"), 0644); err != nil {
		t.Fatal(err)
	}
	called := false
	out, _, err := a.runReviewWith(context.Background(), protocol.Command{ID: "review-on-demand", Workflow: &protocol.WorkflowCommand{Scope: "uncommitted"}}, func(ctx context.Context, cfg config.Config, opts Options, prompt string) (string, error) {
		called = true
		if len(prompt) > 1200 || strings.Contains(prompt, "UNIQUE_SOURCE_SENTINEL") || strings.Contains(prompt, `"before"`) {
			t.Fatal("task still embeds source")
		}
		dir := filepath.Dir(cfg.Workspace)
		if _, err := os.ReadFile(filepath.Join(dir, "changes.patch")); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(dir, "changes.json"))
		if err != nil {
			t.Fatal(err)
		}
		var manifest struct {
			Changes []struct {
				Path   string
				Status string
			}
		}
		if err = json.Unmarshal(data, &manifest); err != nil {
			t.Fatal(err)
		}
		if len(manifest.Changes) != 3 {
			t.Fatalf("wrong selected files: %s", data)
		}
		old, err := os.ReadFile(filepath.Join(dir, "before", "chosen.txt"))
		if err != nil || string(old) != "base\n" {
			t.Fatal("old version unavailable", err)
		}
		next, err := os.ReadFile(filepath.Join(cfg.Workspace, "chosen.txt"))
		if err != nil || string(next) != large {
			t.Fatal("large version unavailable", err)
		}
		if strings.Contains(prompt, "Return only JSON") {
			t.Fatal("JSON-only prompt")
		}
		return "## 发现\n无具体缺陷。\n## 检查范围与限制\n检查三个文件。", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called || !strings.Contains(out, "检查三个文件") {
		t.Fatal(out)
	}
}

func TestReviewCRLFAndCustomFilter(t *testing.T) {
	a, root := codingFixture(t)
	if e := os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("chosen.txt text eol=crlf\nnew.dat filter=unsafe\n"), 0644); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, "chosen.txt"), []byte("base\r\n"), 0644); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, "new.dat"), []byte("not canonical\n"), 0644); e != nil {
		t.Fatal(e)
	}
	cmd := protocol.Command{ID: "crlf-filter", Workflow: &protocol.WorkflowCommand{Scope: "uncommitted", Paths: []string{"chosen.txt", "new.dat"}}}
	copy, e := a.prepareReviewCopy(context.Background(), cmd)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { os.RemoveAll(copy.dir); copy.owner.Close() }()
	_, count, e := copy.task(context.Background(), cmd.Workflow)
	if e != nil || count != 0 || copy.excluded["new.dat"] == "" {
		t.Fatalf("count=%d excluded=%v err=%v", count, copy.excluded, e)
	}
}

func TestReviewCleanupPreservesActiveOwner(t *testing.T) {
	dir, e := os.MkdirTemp("", "ccdp-review-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(dir)
	owner, e := ownReviewDirectory(dir)
	if e != nil {
		t.Fatal(e)
	}
	cleanAbandonedReviews()
	if _, e = os.Stat(dir); e != nil {
		t.Fatal("removed live review", e)
	}
	owner.Close()
	cleanAbandonedReviews()
	if _, e = os.Stat(dir); !os.IsNotExist(e) {
		t.Fatal("orphan not removed", e)
	}
}

func TestReviewSaveRetryDoesNotRunModel(t *testing.T) {
	a, _ := codingFixture(t)
	a.mu.Lock()
	dir := filepath.Join(a.cfg.SessionDir, a.sessionID, "reviews")
	a.mu.Unlock()
	if e := os.MkdirAll(filepath.Dir(dir), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(dir, []byte("block directory"), 0600); e != nil {
		t.Fatal(e)
	}
	state := a.codingState()
	state.pendingReviews = map[string]pendingReview{"retry": {row: session.ReviewRecorded{ID: "retry", Status: "success"}, text: "report body"}}
	if e := a.saveReview("retry"); e == nil {
		t.Fatal("save should fail")
	}
	out, e := a.readReviews(&protocol.WorkflowCommand{Action: "report", ID: "retry"})
	if e != nil || !strings.Contains(out, "report body") {
		t.Fatal(out, e)
	}
	if e = os.Remove(dir); e != nil {
		t.Fatal(e)
	}
	if e = a.saveReview("retry"); e != nil {
		t.Fatal(e)
	}
	out, e = a.readReviews(&protocol.WorkflowCommand{Action: "report", ID: "retry"})
	if e != nil || out != "report body" {
		t.Fatal(out, e)
	}
}
