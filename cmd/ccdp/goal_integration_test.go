package main

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"ccdp/internal/agent"
	"ccdp/internal/config"
	"ccdp/internal/llm"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
)

type goalIntegrationProvider struct{ calls atomic.Int32 }

func (*goalIntegrationProvider) Name() string { return "goal-client-test" }
func (p *goalIntegrationProvider) Stream(ctx context.Context, req llm.CompletionRequest, delta func(string)) (llm.StreamResult, error) {
	if p.calls.Add(1) == 1 {
		delta("work started; ")
		return llm.StreamResult{Text: "work started; ", FinishReason: "stop", PromptTokens: 10, CompletionTok: 5}, nil
	}
	return llm.StreamResult{ToolCalls: []llm.ToolCall{{ID: "finish-goal", Type: "function", Function: llm.Function{Name: "UpdateGoal", Arguments: `{"status":"complete","reason":"implemented and verified"}`}}}, FinishReason: "tool_calls", PromptTokens: 10, CompletionTok: 5}, nil
}

func TestHeadlessGoalRuntimeIntegration(t *testing.T) {
	p := &goalIntegrationProvider{}
	cfg := config.Default()
	cfg.Workspace = t.TempDir()
	cfg.SessionDir = filepath.Join(cfg.Workspace, "sessions")
	cfg.Model = "goal-model"
	cfg.PermissionMode = "bypassPermissions"
	cfg.EnableMemory = config.BoolPtr(false)
	registry := plugin.NewModelRegistry()
	registry.Register(p)
	registry.Route(cfg.Model, p.Name())
	a, err := agent.NewWithOptions(&cfg, agent.Options{EffectiveConfigFrozen: true, ProviderRegistry: registry})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	out, code := captureHeadlessStdout(t, func() int {
		return runHeadlessClientWithGoal(a, nil, outJSON, 10, &protocol.GoalCommand{Action: "set", Objective: "implement feature"})
	})
	if code != 0 || !strings.Contains(out, "implemented and verified") || !strings.Contains(out, `"status": "complete"`) {
		t.Fatalf("code=%d output=%s", code, out)
	}
	if calls := p.calls.Load(); calls != 2 {
		t.Fatalf("model calls=%d, want 2", calls)
	}
}
