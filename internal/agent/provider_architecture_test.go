package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/hooks"
	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/plugin"
	"ccdp/internal/session"
)

type policyTestProvider struct{ name string }

func (p *policyTestProvider) Name() string { return p.name }
func (p *policyTestProvider) Stream(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
	return llm.StreamResult{Text: "ok"}, nil
}

type reloadCapabilityProvider struct{ policyTestProvider }

func (p *reloadCapabilityProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{ToolCalling: true, Images: true, SystemRole: true,
		Usage: true, ContextWindow: 100000, MaxOutputTokens: 10000}
}

type journalRetryProvider struct {
	calls atomic.Int32
}

func (p *journalRetryProvider) Name() string { return "journal-retry" }

func (p *journalRetryProvider) Stream(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
	if p.calls.Add(1) == 1 {
		return llm.StreamResult{}, &llm.RetryableError{Err: errors.New("temporary gateway"), RetryAfter: time.Millisecond}
	}
	return llm.StreamResult{Text: "ok"}, nil
}

type callbackRetryProvider struct {
	calls atomic.Int32
}

func (p *callbackRetryProvider) Name() string { return "callback-retry" }

func (p *callbackRetryProvider) Stream(_ context.Context, _ llm.CompletionRequest, onDelta func(string)) (llm.StreamResult, error) {
	if p.calls.Add(1) == 1 {
		onDelta("partial")
		return llm.StreamResult{}, &llm.RetryableError{Err: errors.New("temporary after delta"), RetryAfter: time.Millisecond}
	}
	return llm.StreamResult{Text: "retry-leak"}, nil
}

func frozenProviderRegistry(model string) *plugin.ModelRegistry {
	r := plugin.NewModelRegistry()
	p := &policyTestProvider{name: "provider-" + model}
	r.Register(p)
	r.Route(model, p.Name())
	return r
}

func isolatedTestConfig(t *testing.T, model string) config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Workspace = dir
	cfg.SessionDir = filepath.Join(dir, "sessions")
	cfg.Model = model
	cfg.BaseURL = "http://127.0.0.1:1/unreachable"
	cfg.PermissionMode = string(permissions.ModeBypass)
	cfg.NoSessionPersistence = true
	cfg.EnableGuardian = config.BoolPtr(false)
	cfg.EnableMemory = config.BoolPtr(false)
	return cfg
}

func TestStreamPreparedRetriesAtJournalBoundary(t *testing.T) {
	a := newRuntimeAgent(t)
	provider := &journalRetryProvider{}
	call, err := llm.NewPreparedCall(provider, provider.Name(), "https://provider.example/v1", llm.CompletionRequest{
		Model:    provider.Name(),
		Messages: []llm.ChatMessage{{Role: "user", Content: "retry once"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.streamPrepared(context.Background(), "main", call, requestJournalMetadata{
		Config: a.configSnapshot(), Turn: 1, Step: 1, HistoryLen: 0,
	}, nil, nil)
	if err != nil {
		t.Fatalf("streamPrepared = %v", err)
	}
	if result.Text != "ok" || provider.calls.Load() != 2 {
		t.Fatalf("retry result=%+v calls=%d, want ok/2", result, provider.calls.Load())
	}
	records, err := a.persistence.store.Read(0)
	if err != nil {
		t.Fatal(err)
	}
	finished := 0
	for _, record := range records {
		if _, ok := record.Event.(*session.AttemptFinished); ok {
			finished++
		}
	}
	if finished != 2 {
		t.Fatalf("journaled AttemptFinished facts=%d, want one per provider attempt", finished)
	}
}

func TestStreamPreparedDoesNotRetryAfterSinkOnlyDelta(t *testing.T) {
	a := newRuntimeAgent(t)
	provider := &callbackRetryProvider{}
	call, err := llm.NewPreparedCall(provider, provider.Name(), "https://provider.example/v1", llm.CompletionRequest{
		Model: provider.Name(), Messages: []llm.ChatMessage{{Role: "user", Content: "sink"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var streamed strings.Builder
	result, err := a.streamPrepared(context.Background(), "main", call, requestJournalMetadata{
		Config: a.configSnapshot(), Turn: 1, Step: 1, HistoryLen: 0,
	}, func(delta string) { streamed.WriteString(delta) }, nil)
	if err == nil || !strings.Contains(err.Error(), "temporary after delta") {
		t.Fatalf("streamPrepared error = %v, want original transient error", err)
	}
	if result.Text != "" || provider.calls.Load() != 1 || streamed.String() != "partial" {
		t.Fatalf("sink-only retry leaked: result=%+v calls=%d streamed=%q", result, provider.calls.Load(), streamed.String())
	}
}

func TestReloadRefreshesActiveProviderOperatorCaps(t *testing.T) {
	cfg := isolatedTestConfig(t, "reload-caps-model")
	cfg.ContextWindow = 8192
	cfg.MaxReplyTokens = 512
	provider := &reloadCapabilityProvider{policyTestProvider{name: "reload-caps-provider"}}
	models := plugin.NewModelRegistry()
	models.Register(provider)
	models.Route(cfg.Model, provider.Name())
	a, err := NewWithOptions(&cfg, nil, Options{EffectiveConfigFrozen: true, ProviderRegistry: models})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// A resolved project/base candidate intentionally changes only operator
	// limits. Reload must rebuild the binding wrapper even though model/provider
	// identity remains unchanged.
	a.mu.Lock()
	a.baseCfg.ContextWindow = 4096
	a.baseCfg.MaxReplyTokens = 128
	a.mu.Unlock()
	candidate, err := a.prepareSettingsCandidate(cfg.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.commitSettingsCandidate(candidate); err != nil {
		t.Fatal(err)
	}
	caps, ok := llm.ProviderCapabilitiesOf(a.activeBinding.client)
	if !ok || caps.ContextWindow != 4096 || caps.MaxOutputTokens != 128 {
		t.Fatalf("active binding capabilities = %+v (described=%v), want 4096/128", caps, ok)
	}
	if a.activeBinding.model != cfg.Model || a.activeBinding.provider != provider.Name() {
		t.Fatalf("reload changed provider identity: %+v", a.activeBinding)
	}
}

func TestNewWithOptionsChildPoliciesAreHardGates(t *testing.T) {
	for _, purpose := range []string{"", childPurposeTask, childPurposeGuardian} {
		t.Run("purpose-"+purpose, func(t *testing.T) {
			cfg := isolatedTestConfig(t, "policy-model")
			registry := frozenProviderRegistry(cfg.Model)
			a, err := NewWithOptions(&cfg, nil, Options{
				EffectiveConfigFrozen: true,
				ProviderRegistry:      registry,
				Purpose:               purpose,
				NonInteractive:        true,
				AllowedTools:          map[string]bool{},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			sentinel := filepath.Join(cfg.Workspace, "must-not-exist")
			args := map[string]any{"file_path": sentinel, "content": "forbidden"}
			_, handled := a.executeTool(messages.ToolCall{Name: "Write", Arguments: args})
			if !handled {
				t.Fatal("hard-denied tool was reported as executable")
			}
			if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("hard-denied tool ran for purpose %q: %v", purpose, err)
			}
		})
	}
}

func TestGuardianOptionsDisableLifecycleSideEffects(t *testing.T) {
	cfg := isolatedTestConfig(t, "guardian-model")
	sentinel := filepath.Join(cfg.Workspace, "guardian-hook-ran")
	cfg.Hooks = hooks.Config{
		hooks.EventSessionStart: {{Command: "touch " + sentinel}},
	}
	a, err := NewWithOptions(&cfg, nil, Options{
		EffectiveConfigFrozen: true,
		ProviderRegistry:      frozenProviderRegistry(cfg.Model),
		Purpose:               childPurposeGuardian,
		NonInteractive:        true,
		AllowedTools:          map[string]bool{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("guardian SessionStart hook ran: %v", err)
	}
	if got := a.MCPServers(); len(got) != 0 {
		t.Fatalf("guardian unexpectedly started MCP servers: %v", got)
	}
}

type guardianReadProvider struct {
	readPath string
	calls    atomic.Int32
}

func (p *guardianReadProvider) Name() string { return "guardian-read-provider" }

func (p *guardianReadProvider) Stream(_ context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
	if p.calls.Add(1) == 1 {
		args, _ := json.Marshal(map[string]any{"file_path": p.readPath})
		return llm.StreamResult{
			FinishReason: "tool_calls",
			ToolCalls: []llm.ToolCall{{ID: "guardian-read", Type: "function", Function: llm.Function{
				Name: "Read", Arguments: llm.ArgumentsJSON(args),
			}}},
		}, nil
	}
	for _, message := range req.Messages {
		if message.Role == "tool" {
			content, _ := message.Content.(string)
			if !strings.Contains(content, "guardian-fixture") {
				return llm.StreamResult{}, errors.New("guardian did not receive the built-in Read result")
			}
		}
	}
	return llm.StreamResult{Text: `{"approved":true,"reason":"read-only inspection"}`}, nil
}

func TestGuardianCannotShadowReadWithConfiguredCommand(t *testing.T) {
	cfg := isolatedTestConfig(t, "guardian-shadow-model")
	fixture := filepath.Join(cfg.Workspace, "guardian-fixture.txt")
	if err := os.WriteFile(fixture, []byte("guardian-fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(cfg.Workspace, "guardian-shadow-ran")
	cfg.Tools = []config.ToolSpec{{Name: "Read", Command: "touch " + sentinel, Permission: "allow"}}
	provider := &guardianReadProvider{readPath: fixture}
	models := plugin.NewModelRegistry()
	models.Register(provider)
	models.Route(cfg.Model, provider.Name())
	parent, err := NewWithOptions(&cfg, nil, Options{
		EffectiveConfigFrozen: true,
		ProviderRegistry:      models,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if _, err := parent.runGuardianChild("inspect the fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("guardian custom Read command ran: %v", err)
	}
	if got := provider.calls.Load(); got != 2 {
		t.Fatalf("guardian provider calls=%d, want initial Read plus verdict", got)
	}
}

func TestTaskRetainsConfiguredCustomTool(t *testing.T) {
	cfg := isolatedTestConfig(t, "task-custom-model")
	sentinel := filepath.Join(cfg.Workspace, "task-custom-ran")
	cfg.Tools = []config.ToolSpec{{Name: "Read", Command: "touch " + sentinel, Permission: "allow"}}
	provider := &policyTestProvider{name: "task-custom-provider"}
	models := plugin.NewModelRegistry()
	models.Register(provider)
	models.Route(cfg.Model, provider.Name())
	task, err := NewWithOptions(&cfg, nil, Options{
		EffectiveConfigFrozen: true,
		ProviderRegistry:      models,
		Purpose:               childPurposeTask,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer task.Close()
	if _, toolErr := task.executeTool(messages.ToolCall{Name: "Read"}); toolErr {
		t.Fatal("configured Task tool was unexpectedly rejected")
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("Task custom tool was not retained: %v", err)
	}
}

func TestNewWithOptionsRejectsUnknownPurposeAndCancelledRoot(t *testing.T) {
	cfg := isolatedTestConfig(t, "validation-model")
	registry := frozenProviderRegistry(cfg.Model)
	if _, err := NewWithOptions(&cfg, nil, Options{
		EffectiveConfigFrozen: true,
		ProviderRegistry:      registry,
		Purpose:               "unknown",
	}); err == nil {
		t.Fatal("unknown child purpose was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewWithOptions(&cfg, nil, Options{
		RootContext:           ctx,
		EffectiveConfigFrozen: true,
		ProviderRegistry:      registry,
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled root was not rejected before construction: %v", err)
	}
}

func TestResolveProviderRefreshesStaleGeneratedHTTPBinding(t *testing.T) {
	cfg := isolatedTestConfig(t, "reload-model")
	cfg.BaseURL = "https://first.example/v1"
	registry := plugin.NewModelRegistry()
	first, firstEndpoint, firstKind, err := resolveProviderForModel(cfg, registry, cfg.Model)
	if err != nil {
		t.Fatal(err)
	}
	if firstKind != "http" || firstEndpoint != cfg.BaseURL {
		t.Fatalf("first binding = %s/%s/%s", first.Name(), firstEndpoint, firstKind)
	}
	cfg.BaseURL = "https://second.example/v1"
	second, secondEndpoint, secondKind, err := resolveProviderForModel(cfg, registry, cfg.Model)
	if err != nil {
		t.Fatal(err)
	}
	if secondKind != "http" || secondEndpoint != cfg.BaseURL || second == first {
		t.Fatalf("stale generated adapter reused: first=%p/%s second=%p/%s", first, firstEndpoint, second, secondEndpoint)
	}
	if got, ok := registry.ResolveDefault(cfg.Model, cfg.BaseURL, cfg.APIKey); !ok || got != second {
		t.Fatalf("registry did not retain refreshed default binding: %v %v", got, ok)
	}
}

func TestPreparedRequestJSONIsStableAfterCustomMarshal(t *testing.T) {
	// Keep a small package-level regression for the journal boundary: this
	// request contains a large integer and custom field order, both of which
	// must survive preparation without aliasing caller memory.
	type custom struct {
		Type  string      `json:"type"`
		Value string      `json:"value"`
		Count json.Number `json:"count"`
	}
	p := &policyTestProvider{name: "prepared"}
	content := &custom{Type: "custom", Value: "before", Count: json.Number("9007199254740993")}
	call, err := llm.NewPreparedCall(p, p.Name(), "", llm.CompletionRequest{
		Model:    "prepared",
		Messages: []llm.ChatMessage{{Role: "user", Content: []any{content}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer call.Close()
	manifest := call.Manifest()
	content.Value = "after"
	if _, err := call.Stream(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	request, err := llm.PreparedRequest(call)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if string(wire) != string(manifest.RequestJSON) {
		t.Fatalf("prepared request changed at provider boundary: %s != %s", wire, manifest.RequestJSON)
	}
}
