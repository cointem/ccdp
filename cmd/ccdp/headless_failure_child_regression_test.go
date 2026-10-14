package main

import (
	"ccdp/internal/agent"
	"ccdp/internal/config"
	"ccdp/internal/llm"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

type reviewFailWithChildProvider struct{}

func (*reviewFailWithChildProvider) Name() string { return "review-fail-child" }
func (*reviewFailWithChildProvider) Stream(ctx context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
	var latest string
	var toolResult bool
	for _, m := range req.Messages {
		if m.Role == "user" {
			latest, _ = m.Content.(string)
		}
		if m.Role == "tool" {
			toolResult = true
		}
	}
	if latest == "child waits" {
		<-ctx.Done()
		return llm.StreamResult{}, ctx.Err()
	}
	if !toolResult {
		return llm.StreamResult{ToolCalls: []llm.ToolCall{{ID: "delegate", Type: "function", Function: llm.Function{Name: "SpawnAgent", Arguments: `{"task":"child waits"}`}}}, FinishReason: "tool_calls"}, nil
	}
	return llm.StreamResult{}, errors.New("terminal provider failure")
}
func TestReviewHeadlessFailureWithChildReturnsFailure(t *testing.T) {
	cfg := config.Default()
	cfg.Workspace = t.TempDir()
	cfg.SessionDir = filepath.Join(cfg.Workspace, "sessions")
	cfg.Model = "review-headless"
	cfg.PermissionMode = "bypassPermissions"
	cfg.EnableMemory = config.BoolPtr(false)
	p := &reviewFailWithChildProvider{}
	r := plugin.NewModelRegistry()
	r.Register(p)
	r.Route(cfg.Model, p.Name())
	a, err := agent.NewWithOptions(&cfg, agent.Options{EffectiveConfigFrozen: true, ProviderRegistry: r})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	out, code := captureHeadlessStdout(t, func() int {
		return runHeadlessClient(protocolOnlyClient{SessionClient: a}, []string{"parent"}, outJSON, 2)
	})
	snapshot, _ := a.Snapshot(context.Background())
	t.Logf("settled parent=%+v pending=%d outstanding=%d", snapshot.LastTurn, len(snapshot.PendingInputs), snapshot.OutstandingWork)
	if snapshot.LastTurn == nil || snapshot.LastTurn.Status != protocol.TurnFailed {
		t.Fatalf("expected settled failed parent: %+v", snapshot.LastTurn)
	}
	if code != 1 || !strings.Contains(out, "terminal provider failure") {
		t.Fatalf("expected prompt provider failure, got code=%d output=%s", code, out)
	}
}
