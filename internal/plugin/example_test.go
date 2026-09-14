package plugin_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"ccdp/internal/events"
	"ccdp/internal/llm"
	"ccdp/internal/plugin"
	"ccdp/internal/tools"
)

// ---------------------------------------------------------------------------
// ExamplePlugin shows how a third-party package would extend ccdp using the
// same surface the built-ins use: register a tool, a Go hook, a provider and a
// session callback, all from one Init. The host wires it in with one Load.
// ---------------------------------------------------------------------------

// ExampleTool is an external tool that would be useful to the agent.
type ExampleTool struct{}

func (ExampleTool) Name() string { return "ExampleEcho" }
func (ExampleTool) Description() string {
	return "Echo a string back; demonstrates how external tools plug in."
}
func (ExampleTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string", "description": "text to echo"},
		},
		"required": []string{"text"},
	}
}
func (ExampleTool) Run(ctx *tools.Context) (string, error) {
	txt, _ := ctx.Args["text"].(string)
	return "echo: " + txt, nil
}

// ExamplePlugin implements the plugin.Plugin contract. It stores the disposers
// returned by each registration and calls them in Deinit — the recommended
// teardown pattern.
type ExamplePlugin struct {
	toolDispose    func()
	hookDispose    func()
	provDispose    func()
	sessionDispose func()
	eventDispose   func()
}

func (ExamplePlugin) Name() string       { return "example" }
func (ExamplePlugin) Requires() []string { return []string{"builtin-tools"} }

func (p *ExamplePlugin) Init(ctx *plugin.Context) error {
	// 1. Register a tool into its own scope (later scopes win on collision).
	p.toolDispose = ctx.Tools.RegisterIn("example", ExampleTool{})
	// 2. Add an in-process pre-tool hook that logs every execution.
	p.hookDispose = ctx.Hooks.AddPreTool(func(name string, args map[string]any) (plugin.ToolDecision, string) {
		return plugin.DecisionNone, ""
	})
	// 3. Register an alternate LLM provider (dynamically removable).
	p.provDispose = ctx.Models.Register(ExampleProvider{name: "example-llm"})
	// 4. Observe session lifecycle.
	p.sessionDispose = ctx.Session.OnCreate(func(id, ws string) {
		_ = id
		_ = ws
	})
	// 5. Subscribe to the tool pipeline event bus.
	p.eventDispose = ctx.Events.Subscribe(events.TopicToolResult, func(topic events.Topic, p events.Payload) {
		if te, ok := p.(events.ToolEvent); ok {
			_ = te.Status
		}
	})
	return nil
}

func (p *ExamplePlugin) Deinit() error {
	p.toolDispose()
	p.hookDispose()
	p.provDispose()
	p.sessionDispose()
	p.eventDispose()
	return nil
}

// ExampleProvider implements llm.Provider.
type ExampleProvider struct{ name string }

func (p ExampleProvider) Name() string { return p.name }
func (p ExampleProvider) Stream(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
	return llm.StreamResult{Text: "example stream"}, nil
}

func TestExternalPluginExtension(t *testing.T) {
	// Wire a host exactly like the agent does.
	bus := events.NewBus()
	reg := tools.NewRegistry()
	ctx := &plugin.Context{
		Events:  bus,
		Tools:   reg,
		Models:  plugin.NewModelRegistry(),
		Hooks:   plugin.NewGoHooks(),
		Session: plugin.NewSessionRegistry(),
	}
	host := plugin.NewHost(ctx)

	// The built-in toolset loads as a plugin; the external one depends on it.
	if err := host.Load(plugin.NewToolsPlugin(false)); err != nil {
		t.Fatalf("builtin-tools: %v", err)
	}

	if err := host.Load(&ExamplePlugin{}); err != nil {
		t.Fatalf("example: %v", err)
	}

	// The external tool is visible through the merged registry.
	tool, ok := reg.Get("ExampleEcho")
	if !ok {
		t.Fatal("ExampleEcho not visible through the merged registry")
	}
	out, err := tool.Run(&tools.Context{Context: context.Background(), Args: map[string]any{"text": "hi"}})
	if err != nil || !strings.Contains(out, "echo: hi") {
		t.Fatalf("external tool run: %q, %v", out, err)
	}

	// The provider is resolvable through an explicit model route.
	ctx.Models.Route("some-model", "example-llm")
	if p, ok := ctx.Models.ResolveRoute("some-model"); !ok || p == nil || p.Name() != "example-llm" {
		t.Fatalf("expected example provider, got %v", p)
	}

	// Plugins are listed in load order: builtin-tools then example.
	if names := host.Names(); len(names) != 2 || names[0] != "builtin-tools" || names[1] != "example" {
		t.Fatalf("unexpected plugin order: %v", names)
	}

	// Unload tears the external plugin down (no dependents remain).
	if err := host.Unload("example"); err != nil {
		t.Fatalf("unload example: %v", err)
	}
	if _, ok := reg.Get("ExampleEcho"); ok {
		t.Fatal("ExampleEcho should be gone after plugin unload")
	}
	fmt.Println("external plugin: OK")
}
