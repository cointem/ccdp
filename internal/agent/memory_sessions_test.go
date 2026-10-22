package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func TestMemorySessionSwitchPreservesChildHistoryArtifactsAndContinuation(t *testing.T) {
	provider := &fakeLLM{script: []string{
		"tool:OutputFixture|",
		"text:retained child answer",
		"text:continued child answer",
	}}
	server := httptest.NewServer(http.HandlerFunc(provider.handler))
	t.Cleanup(server.Close)
	cfg := isolatedTestConfig(t, "memory-child-model")
	cfg.BaseURL = server.URL
	cfg.MaxResultSizeBytes = 1024
	image := inputPNG(t, 123)
	imagePath := filepath.Join(cfg.Workspace, "capture.png")
	if err := os.WriteFile(imagePath, image.Data, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	fullOutput := strings.Repeat("complete child tool output\n", 500)
	a.services.registry.Register(&codeFixtureTool{name: "OutputFixture", text: fullOutput})
	r, err := a.services.supervisor.launch(a, a.Context(), childTask{
		Description: "inspect captured image ![image](capture.png)",
	}, childPurposeTask, "memory-child-call")
	if err != nil {
		t.Fatal(err)
	}
	waitManaged(t, r)
	r.mu.Lock()
	row, runErr := r.fact.Child, r.err
	r.mu.Unlock()
	if runErr != nil {
		t.Fatal(runErr)
	}
	parentID := a.SessionID()
	forkID, err := a.Fork(-1)
	if err != nil {
		t.Fatal(err)
	}
	fork, err := a.OpenSession(forkID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fork.Close)
	if err := a.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(imagePath); err != nil {
		t.Fatal(err)
	}
	reopened, err := fork.OpenSession(parentID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.Close)
	if err := fork.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertChild := func(a *Agent, wantRun protocol.RunID, wantAnswer string) {
		t.Helper()
		children, err := a.Sessions().ListChildren(context.Background())
		if err != nil || len(children) != 1 || children[0].Run.ID != wantRun {
			t.Fatalf("child catalog lost latest run: %+v, %v", children, err)
		}
		page, err := a.Sessions().ReadTranscript(context.Background(), row.SessionID, 0, 64)
		if err != nil {
			t.Fatal(err)
		}
		var answer, toolOutput bool
		for _, item := range page.Items {
			answer = answer || item.Text == wantAnswer
			if item.Tool == "OutputFixture" {
				output, err := a.Sessions().ReadOutput(context.Background(), row.SessionID, item.ID, 0, 64<<10)
				if err != nil || output.Text != fullOutput {
					t.Fatalf("saved child raw output lost: bytes=%d, %v", len(output.Text), err)
				}
				toolOutput = true
			}
		}
		if !answer || !toolOutput {
			t.Fatalf("child transcript lost answer=%v or tool=%v: %+v", answer, toolOutput, page.Items)
		}
	}
	assertChild(reopened, row.Run.ID, "retained child answer")
	next, err := reopened.Sessions().Control(context.Background(), protocol.AgentControl{
		ID: "continue-memory-child", SessionID: row.SessionID, RunID: row.Run.ID,
		Action: "continue", Text: "continue from the saved image and result",
	})
	if err != nil {
		t.Fatal(err)
	}
	continued, err := reopened.services.supervisor.lookup(row.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	waitManaged(t, continued)
	continued.mu.Lock()
	runErr = continued.err
	continued.mu.Unlock()
	if runErr != nil {
		t.Fatal(runErr)
	}
	provider.mu.Lock()
	requests, calls := append([]map[string]any(nil), provider.requests...), provider.calls
	provider.mu.Unlock()
	if calls != 3 {
		t.Fatalf("continuation replayed old work: provider calls=%d", calls)
	}
	wire, err := json.Marshal(requests[2]["messages"])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), "retained child answer") ||
		!strings.Contains(string(wire), base64.StdEncoding.EncodeToString(image.Data)) {
		t.Fatal("continuation request omitted saved answer or frozen image")
	}
	again, err := reopened.OpenSession(parentID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(again.Close)
	if err := reopened.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertChild(again, next.ID, "continued child answer")
	if _, err := os.Stat(cfg.SessionDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("memory recovery touched the session directory: %v", err)
	}
}

func TestMemorySessionRecoversUnacknowledgedChildOutbox(t *testing.T) {
	a, _ := sessionRepairAgent(t, true)
	fact := session.ChildRunRecorded{Version: 1, Child: protocol.ChildSession{
		SessionID: "memory-outbox-child", ParentSessionID: protocol.SessionID(a.SessionID()),
		RootSessionID: protocol.SessionID(a.SessionID()), Purpose: childPurposeTask,
		DelegationID: "memory-outbox-delegation",
		Run:          protocol.RunView{ID: "memory-outbox-run", Status: "running", WaitPolicy: "notify"},
	}}
	if _, err := a.persistenceHandle().CommitEvents("memory-child-running", fact); err != nil {
		t.Fatal(err)
	}
	finished := fact
	finished.Child.Run.Status = "succeeded"
	finished.Child.Run.Output = "completed before parent acknowledgement"
	finished.Child.Run.Usage.InputTokens = 7
	finished.DeliveryID = "child-result-memory-outbox-run"
	s := newSessionSupervisor(a, map[protocol.RunID]memoryRunRecords{
		fact.Child.Run.ID: {records: []session.Record{{Event: &finished}}},
	})
	run, err := s.lookup(fact.Child.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if run.fact.Child.Run.Status != "succeeded" || run.fact.DeliveryID != finished.DeliveryID ||
		run.fact.Child.Run.Usage.InputTokens != 7 || run.fact.UsageAccounted {
		t.Fatalf("memory child outbox not recovered: %+v", run.fact)
	}
}
