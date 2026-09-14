package main

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"ccdp/internal/agent"
	"ccdp/internal/config"
	"ccdp/internal/llm"
	"ccdp/internal/plugin"
)

type backgroundTestProvider struct {
	release chan struct{}
	once    sync.Once
}

func (*backgroundTestProvider) Name() string { return "background-test" }
func (p *backgroundTestProvider) Stream(ctx context.Context, req llm.CompletionRequest, delta func(string)) (llm.StreamResult, error) {
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
	if strings.Contains(latest, "Background subagent") {
		delta("all finished")
		return llm.StreamResult{Text: "all finished", FinishReason: "stop"}, nil
	}
	if latest == "child prompt" {
		select {
		case <-p.release:
		case <-ctx.Done():
			return llm.StreamResult{}, ctx.Err()
		}
		delta("child done")
		return llm.StreamResult{Text: "child done", FinishReason: "stop"}, nil
	}
	if !toolResult {
		return llm.StreamResult{ToolCalls: []llm.ToolCall{{ID: "delegate", Type: "function", Function: llm.Function{Name: "Task", Arguments: `{"description":"child prompt","wait_policy":"notify"}`}}}, FinishReason: "tool_calls"}, nil
	}
	delta("main returned early")
	p.once.Do(func() { close(p.release) })
	return llm.StreamResult{Text: "main returned early", FinishReason: "stop"}, nil
}

func TestHeadlessWaitsForBackgroundDeliveryAndEmitsTypedChildEvents(t *testing.T) {
	for _, format := range []outputFormat{outText, outStreamJSON} {
		t.Run(string(format), func(t *testing.T) {
			p := &backgroundTestProvider{release: make(chan struct{})}
			cfg := config.Default()
			cfg.Workspace = t.TempDir()
			cfg.SessionDir = filepath.Join(cfg.Workspace, "sessions")
			cfg.Model = "background-model"
			cfg.PermissionMode = "bypassPermissions"
			cfg.EnableMemory = config.BoolPtr(false)
			registry := plugin.NewModelRegistry()
			registry.Register(p)
			registry.Route(cfg.Model, p.Name())
			a, err := agent.NewWithOptions(&cfg, nil, agent.Options{EffectiveConfigFrozen: true, ProviderRegistry: registry})
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			out, code := captureHeadlessStdout(t, func() int { return runHeadless(a, []string{"parent prompt"}, format, 10) })
			if code != 0 || !strings.Contains(out, "all finished") {
				t.Fatalf("background task exited early: code=%d %s", code, out)
			}
			if format == outStreamJSON && (!strings.Contains(out, `"type":"child_update"`) || !strings.Contains(out, `"parent_call_id":"delegate"`) || !strings.Contains(out, "child done")) {
				t.Fatalf("missing typed child stream: %s", out)
			}
		})
	}
}
