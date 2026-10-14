package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
)

func TestReviewCancelledSecondWriteHasNoSideEffect(t *testing.T) {
	a, ev := newTestAgent(t, &fakeLLM{script: []string{"tool:Write|file_path=first.txt;content=first", "tool:Write|file_path=late.txt;content=after-stop", "text:done"}})
	n := 0
	a.services.gohooks.AddPreTool(func(name string, args map[string]any) (plugin.ToolDecision, string) {
		if name == "Write" {
			n++
			if n == 2 {
				a.interrupt()
			}
		}
		return plugin.DecisionAllow, "allowed"
	})
	go a.Run()
	submitTestInput(t, a, "write files")
	drainUntil(t, ev, 5*time.Second, func(e protocol.EventView) bool { return e.Kind == protocol.EventTurnDone })
	if _, err := os.Stat(filepath.Join(a.services.cfg.Workspace, "late.txt")); !os.IsNotExist(err) {
		t.Fatalf("file written after cancellation: %v", err)
	}
}

func TestReviewExplorerAdmitsItsAllowedWebTool(t *testing.T) {
	cfg := isolatedTestConfig(t, "explorer")
	cfg.PermissionMode = "bypass"
	cfg.NetworkAccess = true
	cfg.EnableGuardian = config.BoolPtr(false)
	a, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true, Purpose: childPurposeTask, ReadOnlyWorkspace: true, AllowedTools: childReadOnlyTools(childPurposeTask)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	tc := messages.ToolCall{ID: "web", Name: "WebFetch", Arguments: map[string]any{"url": "https://example.com"}}
	_, reason, _, denied := a.hardAdmitTool(tc, toolJournalContext{}, nil)
	if denied {
		t.Fatalf("explorer WebFetch blocked despite explicit allowed set: %s", reason)
	}
}

type reviewCancelGuardianProvider struct{ cancel func() }

func (p *reviewCancelGuardianProvider) Name() string { return "cancel-guardian" }

func (p *reviewCancelGuardianProvider) Stream(_ context.Context, _ llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
	p.cancel()
	return llm.StreamResult{Text: `{"approved":true}`, FinishReason: "stop"}, nil
}

func TestReviewCancellationDuringGuardianHasNoWrite(t *testing.T) {
	cfg := isolatedTestConfig(t, "cancel-guardian")
	cfg.EnableGuardian = config.BoolPtr(true)
	p := &reviewCancelGuardianProvider{}
	registry := plugin.NewModelRegistry()
	registry.Register(p)
	registry.Route(cfg.Model, p.Name())
	a, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true, ProviderRegistry: registry})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	a.mu.Lock()
	a.turn.turnCtx, a.turn.turnCancel = context.WithCancel(a.lifecycle.rootCtx)
	a.mu.Unlock()
	p.cancel = a.interrupt
	path := filepath.Join(cfg.Workspace, "after-cancel.txt")
	out, failed := a.executeTool(messages.ToolCall{ID: "write", Name: "Write", Arguments: map[string]any{"file_path": path, "content": "after cancellation"}})
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("guardian cancellation still wrote file: failed=%v output=%s stat=%v", failed, out, err)
	}
}
