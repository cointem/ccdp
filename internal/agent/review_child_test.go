package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func TestBackgroundReviewIndependentOfParent(t *testing.T) {
	for _, cancelReview := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "cancel"}[cancelReview], func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			var once, startOnce sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			mainModel := &fakeLLM{script: []string{"text:main answer"}}
			reviewModel := &fakeLLM{script: []string{`text:## 发现
未发现具体缺陷。`, `text:根据已保存报告解释检查范围。`}}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				r.Body = io.NopCloser(bytes.NewReader(body))
				if bytes.Contains(body, []byte("explain saved report")) {
					var request struct {
						Tools []json.RawMessage `json:"tools"`
					}
					if e := json.Unmarshal(body, &request); e != nil || len(request.Tools) != 0 {
						t.Error("report discussion still exposes tools", e)
					}
				}
				if bytes.Contains(body, []byte("../changes.patch")) {
					startOnce.Do(func() { close(started) })
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					reviewModel.handler(w, r)
				} else {
					mainModel.handler(w, r)
				}
			}))
			defer server.Close()
			root := t.TempDir()
			git(t, root, "init", "-q")
			git(t, root, "config", "user.name", "Test")
			git(t, root, "config", "user.email", "test@example.invalid")
			path := filepath.Join(root, "a.go")
			if err := os.WriteFile(path, []byte("package p\nfunc A() int { return 0 }\n"), 0644); err != nil {
				t.Fatal(err)
			}
			git(t, root, "add", "a.go")
			git(t, root, "-c", "commit.gpgsign=false", "commit", "-qm", "base")
			if err := os.WriteFile(path, []byte("package p\nfunc A() int { return 1 }\n"), 0644); err != nil {
				t.Fatal(err)
			}
			cfg := config.Default()
			cfg.Workspace, cfg.SessionDir, cfg.BaseURL = root, t.TempDir(), server.URL
			cfg.APIKey, cfg.Model, cfg.PermissionMode = "test", "fake", "bypass"
			cfg.EnableGuardian, cfg.EnableMemory, cfg.AutomaticCheckpoints = config.BoolPtr(false), config.BoolPtr(false), config.BoolPtr(false)
			cfg.MCPServers = nil
			a, err := New(&cfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := protocol.Command{ID: "background-review", SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandRunWorkflow,
				Workflow: &protocol.WorkflowCommand{Kind: protocol.WorkflowReview, Scope: "uncommitted"}}
			submitWorkflow(t, a, cmd)
			waitWorkflowCompleted(t, a, cmd.ID)
			launchResult, err := a.ReadOperation(ctx, cmd.ID)
			if err != nil {
				t.Fatal(err)
			}
			var launched reviewLaunch
			if err := json.Unmarshal([]byte(launchResult.Output), &launched); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("review did not start")
			}
			view, err := a.Snapshot(ctx)
			if err != nil || view.Busy {
				t.Fatalf("review reserved parent: busy=%v err=%v", view.Busy, err)
			}
			if err := os.WriteFile(path, []byte("package p\nfunc A() int { return 2 }\n"), 0644); err != nil {
				t.Fatal(err)
			}
			rows, err := a.Sessions().ListChildren(ctx)
			if err != nil || len(rows) != 1 {
				t.Fatalf("children: %v %v", rows, err)
			}
			snapshotData, err := os.ReadFile(filepath.Join(rows[0].Workspace, "a.go"))
			if err != nil || !strings.Contains(string(snapshotData), "return 1") {
				t.Fatal("review snapshot changed with parent workspace", err)
			}
			receipt, err := a.Submit(ctx, protocol.NewSubmitInput("main-input", cmd.SessionID, "main-input", "continue main work", protocol.InputSteer))
			if err != nil || receipt.Rejected() {
				t.Fatalf("main input: %v %v", receipt, err)
			}
			for {
				view, err = a.Snapshot(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if !view.Busy && view.LastTurn != nil {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if view.LastTurn.Status != protocol.TurnSucceeded {
				t.Fatalf("parent outcome: %+v", view.LastTurn)
			}
			if cancelReview {
				_, err = a.Sessions().Control(ctx, protocol.AgentControl{ID: "cancel-review", SessionID: launched.SessionID, RunID: launched.RunID, Action: "cancel"})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				unblock()
			}
			if err := a.WaitChildren(ctx); err != nil {
				t.Fatal(err)
			}
			result, err := a.ReadReviewOperation(ctx, cmd.ID)
			if err != nil || !result.Complete {
				t.Fatalf("review completion: %+v %v", result, err)
			}
			if (result.Status == "success") == cancelReview {
				t.Fatalf("review outcome: %+v", result)
			}
			mainModel.mu.Lock()
			calls := mainModel.calls
			mainModel.mu.Unlock()
			if calls != 1 || len(a.History()) != 2 {
				t.Fatalf("review caused a parent follow-up: requests=%d history=%d", calls, len(a.History()))
			}
			reader, err := a.Sessions().OpenReader(ctx, launched.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			childView, err := reader.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, item := range childView.Transcript {
				if strings.HasPrefix(item.ID, "review-report-") {
					found = true
				}
			}
			if !found {
				t.Fatal("child has no saved final report")
			}
			if !cancelReview {
				if _, err := os.Stat(rows[0].Workspace); !os.IsNotExist(err) {
					t.Fatal("review copy not cleaned", err)
				}
				if _, err = a.Sessions().Control(ctx, protocol.AgentControl{ID: "discuss-review", SessionID: launched.SessionID, RunID: launched.RunID, Action: "continue", Text: "explain saved report"}); err != nil {
					t.Fatal("cannot discuss cleaned review", err)
				}
				if err = a.WaitChildren(ctx); err != nil {
					t.Fatal(err)
				}
				list, err := a.Sessions().ListChildren(ctx)
				if err != nil || list[0].Run.Status != "succeeded" || list[0].Run.WaitPolicy != "observe" {
					t.Fatal("review discussion failed", list, err)
				}

			}

		})
	}
}

func TestReviewChildReportSurvivesRestart(t *testing.T) {
	a, _ := codingFixture(t)
	out, err := runCoding(t, a, protocol.WorkflowCommand{Kind: protocol.WorkflowReview, Scope: "uncommitted"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.WaitChildren(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := a.Sessions().ListChildren(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("children: %v %v", rows, err)
	}
	cfg, id := cloneConfig(a.cfg), a.SessionID()
	a.Close()
	snap, err := LoadSession(cfg.SessionDir, id)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := Resume(&cfg, snap, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	reader, err := resumed.Sessions().OpenReader(context.Background(), rows[0].SessionID)
	if err != nil {
		t.Fatal(err)
	}
	view, err := reader.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range view.Transcript {
		if strings.HasPrefix(item.ID, "review-report-") && item.Text == out {
			return
		}
	}
	t.Fatal("saved review report was lost after restart")
}

func TestReviewApprovalDoesNotReplaceParentDecision(t *testing.T) {
	a, _ := codingFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	close(done)
	sid := protocol.SessionID(newSessionID())
	r := &managedRun{parent: a, done: done, cancel: func() {}, opts: Options{Permissions: permissions.NewManager(permissions.ModeDefault, permissions.Policy{})},
		fact: session.ChildRunRecorded{Child: protocol.ChildSession{SessionID: sid, ParentSessionID: protocol.SessionID(a.SessionID()), Purpose: childPurposeReview, Run: protocol.RunView{ID: "approval-review", Status: "starting", WaitPolicy: "observe"}}}}
	a.supervisor.mu.Lock()
	a.supervisor.children[sid] = r
	a.supervisor.mu.Unlock()
	defer func() { a.supervisor.mu.Lock(); delete(a.supervisor.children, sid); a.supervisor.mu.Unlock() }()
	a.mu.Lock()
	a.pendingApproval = &ApprovalRequest{ID: "main-decision", Tool: "Bash"}
	a.mu.Unlock()
	result := make(chan error, 1)
	go func() {
		result <- a.supervisor.authorizeReviewGit(r, ctx, map[string]any{"command": "requires-explicit-approval"})
	}()
	var approvalID string
	for approvalID == "" {
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		r.mu.Lock()
		if r.fact.Child.Approval != nil {
			approvalID = r.fact.Child.Approval.ID
		}
		r.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	reader, err := a.Sessions().OpenReader(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	view, err := reader.Snapshot(ctx)
	if err != nil || view.Approval == nil || view.Approval.ID != approvalID {
		t.Fatalf("child approval not visible: %+v %v", view.Approval, err)
	}
	_, err = a.Sessions().Control(ctx, protocol.AgentControl{ID: "approve-review", SessionID: sid, RunID: "approval-review", Action: "approve", ApprovalID: approvalID, Approve: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	unchanged := a.pendingApproval != nil && a.pendingApproval.ID == "main-decision"
	a.pendingApproval = nil
	a.mu.Unlock()
	if !unchanged {
		t.Fatal("review replaced the main decision")
	}
}
