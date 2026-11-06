package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"ccdp/internal/config"
	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/plugin"
	"ccdp/internal/session/archive"
	"ccdp/internal/tools"
)

func TestComputerLaunchGrantAndRootIsolation(t *testing.T) {
	a := newAgentForTest(t)
	defer a.Close()
	if _, ok := a.services.registry.Get("Computer"); ok {
		t.Fatal("desktop enabled by default")
	}
	a.services.registry.RegisterIn("test", &tools.ComputerTool{})
	step := testToolStep(t, a)
	call := messages.ToolCall{ID: "desktop", Name: "Computer", Arguments: map[string]any{"operation": "status"}}
	j := toolJournalContextForStep(step)
	if _, _, _, denied := a.hardAdmitTool(call, j); !denied {
		t.Fatal("missing launch grant accepted")
	}
	a.mu.Lock()
	a.services.cfg.ComputerUseEnabled = true
	a.mu.Unlock()
	if _, _, _, denied := a.hardAdmitTool(call, j); denied {
		t.Fatal("explicit desktop grant denied")
	}
	for _, mode := range []permissions.Mode{permissions.ModeAuto, permissions.ModeManual} {
		m := permissions.NewManager(mode, permissions.Policy{})
		decision, _ := m.Check("Computer", call.Arguments, toolKind(&tools.ComputerTool{}))
		if (decision == permissions.DecisionAllow) != (mode == permissions.ModeAuto) {
			t.Fatalf("wrong desktop policy for %s: %s", mode, decision)
		}
		m.SetPolicy(permissions.Policy{AlwaysAllow: []string{"Computer"}, AlwaysDeny: []string{"Computer"}})
		if decision, _ := m.Check("Computer", call.Arguments, permissions.KindComputer); decision != permissions.DecisionDeny {
			t.Fatal("hard deny bypassed")
		}
	}
	childCfg, _, err := childOptionsFromParent(a, childPurposeTask)
	if err != nil {
		t.Fatal(err)
	}
	if childCfg.ComputerUseEnabled {
		t.Fatal("child inherited desktop grant")
	}
	// A borrowed concrete implementation must also be rejected, even if a
	// caller constructs a child with an erroneously enabled config.
	a.mu.Lock()
	a.children.childState = &childRuntimeState{purpose: childPurposeTask}
	a.mu.Unlock()
	if _, _, _, denied := a.hardAdmitTool(call, j); !denied {
		t.Fatal("child obtained desktop capability")
	}
	a.mu.Lock()
	a.children.childState = nil
	a.mu.Unlock()
	cfg := a.configSnapshot()
	wire, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var restored config.Config
	if err := json.Unmarshal(wire, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.ComputerUseEnabled || !cfg.Clone().ComputerUseEnabled {
		t.Fatal("launch grant persisted or was lost by an in-process clone")
	}
}

type imageFixtureTool struct{}

func (*imageFixtureTool) Name() string        { return "FormalAuditEffect" }
func (*imageFixtureTool) Description() string { return "fixture" }
func (*imageFixtureTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (*imageFixtureTool) Execute(*tools.Context, map[string]any) (tools.Result, error) {
	return tools.Result{Text: `{"frame":{"id":"fixture"}}`, Image: &messages.ImageAttachment{MediaType: "image/jpeg", Data: []byte("frozen desktop bytes")}}, nil
}

type imageRoundProvider struct {
	calls int
	image bool
}

func (*imageRoundProvider) Name() string { return "image-round" }
func (p *imageRoundProvider) Stream(_ context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
	p.calls++
	if p.calls == 1 {
		return llm.StreamResult{ToolCalls: []llm.ToolCall{{ID: "screen", Type: "function", Function: llm.Function{Name: "FormalAuditEffect", Arguments: llm.ArgumentsJSON(`{}`)}}}, FinishReason: "tool_calls"}, nil
	}
	p.image = requestImageCount(req) == 1
	return llm.StreamResult{Text: "done", FinishReason: "stop"}, nil
}
func requestImageCount(req llm.CompletionRequest) int {
	// Providers may receive typed parts or the equivalent JSON-decoded arrays.
	wire, _ := json.Marshal(req)
	return strings.Count(string(wire), "data:image/jpeg;base64,")
}

func TestComputerImagePersistsThroughRequestResumeAndFork(t *testing.T) {
	p := &imageRoundProvider{}
	a := newJournalRuntimeAgent(t, p)
	a.services.registry.RegisterIn("test", &imageFixtureTool{})
	formalCompletionTurn(t, a, "image-round", "inspect desktop fixture")
	if p.calls != 2 || !p.image {
		t.Fatalf("model did not receive the tool image on its next request: calls=%d image=%t", p.calls, p.image)
	}
	cfg := a.configSnapshot()
	forkID, err := a.Fork(len(a.History()))
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	for _, id := range []string{a.SessionID(), forkID} {
		snapshot, err := archive.Load(cfg.SessionDir, id)
		if err != nil {
			t.Fatal(err)
		}
		cfg.SessionID = id
		models := plugin.NewModelRegistry()
		models.Register(p)
		models.Route(cfg.Model, p.Name())
		r, err := newAgentWithOptions(&cfg, snapshot, Options{EffectiveConfigFrozen: true, ProviderRegistry: models})
		if err != nil {
			t.Fatal(err)
		}
		req := r.buildRequestFromConfig(cfg, r.currentSandbox(), cfg.Model, "test", r.History(), nil)
		if requestImageCount(req) != 1 {
			t.Fatalf("session %s lost frozen image", id)
		}
		r.Close()
	}
}

func TestComputerRequestKeepsOnlyTwoRecentScreenshots(t *testing.T) {
	a := newAgentForTest(t)
	defer a.Close()
	ref, err := a.persistenceHandle().PutBlob(context.Background(), []byte("fixture"), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	var history []messages.Message
	for i := 0; i < 5; i++ {
		history = append(history, messages.Message{Role: messages.RoleUser, InternalSource: "runtime_computer", Content: "![screen](screen.jpg)", ImagesFrozen: true, ImageAttachments: []messages.ImageAttachment{{Path: "screen.jpg", MediaType: ref.MediaType, BlobHash: ref.Hash, BlobSize: ref.Size}}})
	}
	cfg := a.configSnapshot()
	req := a.buildRequestFromConfig(cfg, a.currentSandbox(), cfg.Model, "test", history, nil)
	if requestImageCount(req) != 2 || len(history) != 5 {
		t.Fatal("screenshot window exceeded or durable history mutated")
	}
	wire, _ := json.Marshal(req)
	if strings.Count(string(wire), "data:image/jpeg") != 2 {
		t.Fatal("base64 images leaked outside image blocks")
	}
}
