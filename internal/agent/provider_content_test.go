package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/plugin"
	"ccdp/internal/session"
)

func TestProviderContentSurvivesDurableRequestReconstruction(t *testing.T) {
	a := newAgentForTest(t)
	defer a.Close()
	original := messages.Message{Role: messages.RoleAssistant, Content: "before tool", ReasoningContent: "reason", ProviderOrigin: "stable-source", ProviderWire: "anthropic", ProviderContent: []json.RawMessage{
		json.RawMessage(`{"type":"thinking","thinking":"reason","signature":"signed"}`),
		json.RawMessage(`{"type":"redacted_thinking","data":"opaque"}`),
		json.RawMessage(`{"type":"text","text":"before tool"}`),
		json.RawMessage(`{"type":"tool_use","id":"call","name":"Read","input":{"file_path":"a"}}`),
	}, ToolCalls: []messages.ToolCall{{ID: "call", Name: "Read", Arguments: map[string]any{"file_path": "a"}}}}
	stored, err := messageToSession(original)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	var disk session.Message
	if err = json.Unmarshal(encoded, &disk); err != nil {
		t.Fatal(err)
	}
	restored, err := session.DecodeMessage(disk)
	if err != nil {
		t.Fatal(err)
	}
	history := messages.Clone([]messages.Message{restored, messages.NewToolResult(original.ToolCalls[0], "contents", false)})
	cfg := a.configSnapshot()
	cfg.WireAPI = "anthropic"
	req := a.buildRequestFromConfig(cfg, cfg.Model, "test", history, false, nil)
	found := false
	for _, message := range req.Messages {
		if message.Role == "assistant" {
			found = true
			if message.ProviderOrigin != "stable-source" || message.ProviderWire != "anthropic" || !reflect.DeepEqual(message.ProviderContent, original.ProviderContent) {
				t.Fatalf("reconstructed request lost blocks: %+v", message)
			}
		}
	}
	if !found {
		t.Fatal("missing assistant")
	}
	stored.ProviderContent[0][0] = 'x'
	disk.ProviderContent[1][0] = 'x'
	restored.ProviderContent[2][0] = 'x'
	if !json.Valid(original.ProviderContent[0]) || !json.Valid(history[0].ProviderContent[1]) || !json.Valid(history[0].ProviderContent[2]) {
		t.Fatal("provider blocks alias session/history copies")
	}
}

func TestCompactionPreservesIncompleteProviderToolRound(t *testing.T) {
	history := []messages.Message{{Role: messages.RoleUser, Content: "old"}, {Role: messages.RoleAssistant, Content: "old answer"}, {Role: messages.RoleUser, Content: "new"}, {Role: messages.RoleAssistant, ProviderWire: "anthropic", ProviderContent: []json.RawMessage{json.RawMessage(`{"type":"thinking","thinking":"step","signature":"signed"}`)}, ToolCalls: []messages.ToolCall{{ID: "a"}, {ID: "b"}}}, {Role: messages.RoleTool, ToolCallID: "a", Content: "only first result"}}
	cut := compactCut(history, 0, 0)
	if cut > 3 {
		t.Fatalf("cut %d split pending tool round", cut)
	}
}

type opaqueRoundProvider struct{ calls atomic.Int32 }

func (p *opaqueRoundProvider) Name() string { return "opaque-round-provider" }

func (p *opaqueRoundProvider) Stream(_ context.Context, _ llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
	if p.calls.Add(1) == 1 {
		return llm.StreamResult{Text: "before tool", Reasoning: "reason", ProviderOrigin: "stable-source", ProviderWire: "anthropic", ProviderContent: []json.RawMessage{
			json.RawMessage(`{"type":"thinking","thinking":"reason","signature":"signed"}`), json.RawMessage(`{"type":"redacted_thinking","data":"opaque"}`), json.RawMessage(`{"type":"text","text":"before tool"}`), json.RawMessage(`{"type":"tool_use","id":"read","name":"Read","input":{"file_path":"input.txt"}}`),
		}, ToolCalls: []llm.ToolCall{{ID: "read", Type: "function", Function: llm.Function{Name: "Read", Arguments: llm.ArgumentsJSON(`{"file_path":"input.txt"}`)}}}, FinishReason: "tool_calls"}, nil
	}
	return llm.StreamResult{Text: "done", FinishReason: "stop"}, nil
}

func TestProviderContentAgentResumeAndFork(t *testing.T) {
	provider := &opaqueRoundProvider{}
	a := newJournalRuntimeAgent(t, provider)
	cfg := a.configSnapshot()
	if err := os.WriteFile(filepath.Join(cfg.Workspace, "input.txt"), []byte("contents"), 0600); err != nil {
		t.Fatal(err)
	}
	formalCompletionTurn(t, a, "opaque-round", "read input.txt")
	if provider.calls.Load() != 2 {
		t.Fatalf("tool round did not continue: %d", provider.calls.Load())
	}
	child, err := a.Fork(-1)
	if err != nil {
		t.Fatal(err)
	}
	originalID := a.SessionID()
	if err := a.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{originalID, child} {
		snapshot, err := LoadSession(cfg.SessionDir, id)
		if err != nil {
			t.Fatal(err)
		}
		cfg.SessionID = id
		registry := plugin.NewModelRegistry()
		registry.Register(provider)
		registry.Route(cfg.Model, provider.Name())
		resumed, err := newAgentWithOptions(&cfg, snapshot, Options{EffectiveConfigFrozen: true, ProviderRegistry: registry})
		if err != nil {
			t.Fatal(err)
		}
		req := resumed.buildRequestFromConfig(cfg, cfg.Model, "resumed", resumed.History(), false, nil)
		found := false
		for _, message := range req.Messages {
			if len(message.ProviderContent) > 0 {
				found = true
				if message.ProviderOrigin != "stable-source" || message.ProviderWire != "anthropic" || len(message.ProviderContent) != 4 || !strings.Contains(string(message.ProviderContent[0]), "signed") || !strings.Contains(string(message.ProviderContent[1]), "opaque") {
					t.Fatalf("resume/fork lost ordered provider blocks: %+v", message)
				}
			}
		}
		if !found {
			t.Fatalf("session %s lost provider blocks", id)
		}
		_ = resumed.CloseContext(context.Background())
	}
}

func TestHistorySanitizerKeepsOpaqueToolRoundPaired(t *testing.T) {
	raw := []json.RawMessage{json.RawMessage(`{"type":"thinking","thinking":"plan","signature":"signed"}`), json.RawMessage(`{"type":"tool_use","id":"kept","name":"Read","input":{}}`)}
	history := []messages.Message{{Role: messages.RoleUser, Content: "complete prefix"}, {Role: messages.RoleAssistant, ProviderWire: "anthropic", ProviderContent: raw, ToolCalls: []messages.ToolCall{{ID: "kept", Name: "Read"}}}}
	got := sanitizeToolPairs(history)
	if len(got) != 3 || got[2].ToolCallID != "kept" || !reflect.DeepEqual(got[1].ProviderContent, raw) {
		t.Fatalf("sanitizer damaged opaque tool round: %+v", got)
	}
	forked := cloneForkHistory(history)
	if len(forked) != 1 {
		t.Fatalf("fork kept incomplete signed tool round: %+v", forked)
	}
	history = append(history, messages.Message{Role: messages.RoleTool, ToolCallID: "kept", Content: "result"})
	forked = cloneForkHistory(history)
	if len(forked) != 3 || !reflect.DeepEqual(forked[1].ProviderContent, raw) {
		t.Fatalf("fork changed complete signed tool round: %+v", forked)
	}
}
